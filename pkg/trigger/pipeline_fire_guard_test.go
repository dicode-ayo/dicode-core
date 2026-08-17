package trigger

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
)

// TestFireGuardVetoesPipelineChainFire is the regression test for #678: a
// pipeline chain-triggered off an upstream task's completion used to fire via
// a direct e.firePipeline(...) call in firePipelineChains, bypassing
// checkFireGuard entirely. A pipeline the Approval Gate is holding Pending
// (unarmed) would still run when the chain edge fired. firePipelineChains now
// routes through fireKinded like every other fire path, so the veto applies
// here too.
func TestFireGuardVetoesPipelineChainFire(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	stage := writeTask(t, dir, "guarded-chain-stage",
		`export default async function main() { return "ok" }`, task.TriggerConfig{Manual: true})
	upstream := writeTask(t, dir, "guarded-chain-up",
		`export default async function main() { return "done" }`, task.TriggerConfig{Manual: true})
	for _, s := range []*task.Spec{stage, upstream} {
		if err := env.reg.Register(s); err != nil {
			t.Fatal(err)
		}
		if err := env.engine.Register(s); err != nil {
			t.Fatal(err)
		}
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "guarded-chained-pipe", Name: "GCP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Chain: &task.ChainTrigger{From: "guarded-chain-up", On: "success"}},
		Stages:  []task.Stage{{Task: "guarded-chain-stage"}},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}

	// Hold the pipeline Pending: veto any fire of its task ID. guardSeen
	// signals that firePipelineChains's goroutine actually reached the guard,
	// so the assertion below doesn't race a dispatch that simply hasn't run
	// yet — a reintroduced bypass must be caught deterministically, not by
	// outlasting a fixed polling window.
	guardSeen := make(chan struct{}, 1)
	env.engine.SetFireGuard(func(taskID string) error {
		if taskID == "guarded-chained-pipe" {
			select {
			case guardSeen <- struct{}{}:
			default:
			}
			return fmt.Errorf("task pending approval: %s", taskID)
		}
		return nil
	})

	if _, err := env.engine.FireManual(context.Background(), "guarded-chain-up", nil); err != nil {
		t.Fatalf("FireManual up: %v", err)
	}

	// The chain edge fires once the upstream run completes.
	upRun := findRun(t, env, "guarded-chain-up", registry.RunKindTask, 30*time.Second)
	if upRun.Status != registry.StatusSuccess {
		t.Fatalf("upstream run status = %q, want success", upRun.Status)
	}

	// firePipelineChains dispatches the vetoed fire from a goroutine; wait for
	// it to actually reach the guard before asserting on run records, rather
	// than racing a fixed polling window.
	select {
	case <-guardSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("chain dispatch did not reach the fire guard")
	}
	runs, err := env.reg.ListRuns(context.Background(), "guarded-chained-pipe", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("pending pipeline fired via chain edge despite fire guard veto: %d run(s) found", len(runs))
	}
}

// TestFireGuardVetoesPipelineWebhookFire is the webhook-half of the #678
// regression: the pipeline webhook handler used to fire via a direct
// e.firePipeline(...) call, bypassing checkFireGuard. A POST to a Pending
// pipeline's own webhook would still start it. The handler now routes through
// fireKinded, so a fire-guard veto rejects the request (500, guard error
// surfaced in the body) and no run row is created.
func TestFireGuardVetoesPipelineWebhookFire(t *testing.T) {
	env := newTestEnv(t)
	stage := &task.Spec{ID: "guarded-webhook-stage", Name: "S", Enabled: true,
		Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	if err := env.reg.Register(stage); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(stage); err != nil {
		t.Fatal(err)
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "guarded-webhook-pipe", Name: "GWP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Webhook: "/hooks/guarded-webhook-pipe"},
		Stages:  []task.Stage{{Task: "guarded-webhook-stage"}},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}

	// Hold the pipeline Pending: veto any fire of its task ID.
	env.engine.SetFireGuard(func(taskID string) error {
		if taskID == "guarded-webhook-pipe" {
			return fmt.Errorf("task pending approval: %s", taskID)
		}
		return nil
	})

	handler := env.engine.WebhookHandler()
	req := httptest.NewRequest(http.MethodPost, "/hooks/guarded-webhook-pipe", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("webhook POST to a pending pipeline status = %d, want %d: %s",
			w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pending approval") {
		t.Fatalf("response body = %q, want it to surface the fire guard veto", w.Body.String())
	}
	if w.Header().Get("X-Run-Id") != "" {
		t.Fatalf("expected no X-Run-Id header on a vetoed fire, got %q", w.Header().Get("X-Run-Id"))
	}

	runs, err := env.reg.ListRuns(context.Background(), "guarded-webhook-pipe", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("pending pipeline fired via its own webhook despite fire guard veto: %d run(s) found", len(runs))
	}
}

// TestFireGuardVetoesDirectFirePipelineCall closes the architectural gap noted
// in review of #704 (PR comment on #704): firePipeline had no fire-guard check
// of its own, so pipeline safety depended entirely on every caller remembering
// to route through fireKinded first. This calls e.firePipeline directly
// (bypassing fireKinded's own checkFireGuard call entirely) to prove the guard
// is enforced by firePipeline itself, not merely by its callers.
func TestFireGuardVetoesDirectFirePipelineCall(t *testing.T) {
	env := newTestEnv(t)
	stage := &task.Spec{ID: "guarded-direct-stage", Name: "S", Enabled: true,
		Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	if err := env.reg.Register(stage); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(stage); err != nil {
		t.Fatal(err)
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "guarded-direct-pipe", Name: "GDP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages:  []task.Stage{{Task: "guarded-direct-stage"}},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}

	// Hold the pipeline Pending: veto any fire of its task ID.
	env.engine.SetFireGuard(func(taskID string) error {
		if taskID == "guarded-direct-pipe" {
			return fmt.Errorf("task pending approval: %s", taskID)
		}
		return nil
	})

	// Call firePipeline directly, not through fireKinded, to exercise
	// firePipeline's own internal checkFireGuard call in isolation.
	runID, err := env.engine.firePipeline(context.Background(), pipe, pkgruntime.RunOptions{}, registry.TriggerManual)
	if err == nil || !strings.Contains(err.Error(), "pending approval") {
		t.Fatalf("firePipeline called directly = (%q, %v), want a pending-approval veto error", runID, err)
	}
	if runID != "" {
		t.Fatalf("firePipeline returned a non-empty run ID on a vetoed fire: %q", runID)
	}

	runs, err := env.reg.ListRuns(context.Background(), "guarded-direct-pipe", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("firePipeline created a run row despite fire guard veto: %d run(s) found", len(runs))
	}
}

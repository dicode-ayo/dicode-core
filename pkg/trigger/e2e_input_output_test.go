package trigger

// End-to-end coverage for PR #310's `${input.output}` interpolation
// primitive on the success-chain dispatch edge, exercised through the
// REAL Deno runtime (not test-exec mocks).
//
// The companion tests in engine_chain_params_test.go already cover the
// same primitive at the unit + integration-with-mocks layer:
//   - TestChainDispatch_ResolvesInputOutput uses an echoing downstream
//     to assert the resolved value lands in `input`.
//   - TestOnFailureChainDispatch_ResolvesInputOutput drives FireChain
//     directly with a synthetic output to pin the failure-chain edge.
//   - TestOnFailureChainDispatch_NonStringUpstreamSkips pins the
//     short-circuit on non-string upstreams.
//
// What this e2e adds on top of those:
//
//  1. The DOWNSTREAM is a real Deno task that observes the resolved
//     value by writing a marker file rather than echoing input as the
//     return value. That closes the loop on the substituted value
//     actually being visible to a task body, not just persisted in
//     `runs.return_value`.
//
//  2. The UPSTREAM in the happy-path + embedded-token cases is the
//     ACTUAL `tasks/buildin/template/` task loaded from disk — the same
//     spec/code that ships in dicode-core. That exercises the real
//     library task's `run_result.enabled: false` + in-memory cache
//     return path PR #310 was designed for, and provides regression
//     coverage if buildin/template's contract drifts.
//
//  3. The non-string-upstream short-circuit is verified with an INLINE
//     Deno task returning a JSON object — buildin/template returns only
//     strings, so the negative case needs a separate fixture.
//
// PR #3 in the same epic covers `${input.output}` on `trigger.before`
// overrides; that's out of scope here. We only cover the chain edge.
//
// Gated on real Deno (skipped via newTestEnv's t.Skipf) — same gate
// as TestE2E_TemplatePreflightPipeline.

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// writeMarkerTask emits a Deno task with marker-file write permission
// scoped to markerDir and MARKER_PATH in --allow-env. The body is
// caller-supplied so each test can encode its own assertion shape.
// run_result.enabled defaults to true; pass disablePersist=true to opt
// the in-memory cache path that buildin/template + buildin/write-local
// use.
func writeMarkerTask(t *testing.T, dir, id, body, markerDir string, trigger task.TriggerConfig, disablePersist bool) *task.Spec {
	t.Helper()
	td := filepath.Join(dir, id)
	if err := os.MkdirAll(td, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "task.yaml"), []byte("name: "+id+"\nruntime: deno\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "task.ts"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDeno,
		Trigger: trigger,
		Timeout: 30 * time.Second,
		TaskDir: td,
		Enabled: true,
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: "MARKER_PATH"}},
			FS:  []task.FSEntry{{Path: markerDir, Permission: "rw"}},
		},
	}
	if disablePersist {
		disabled := false
		spec.RunResult = &task.RunResultConfig{Enabled: &disabled}
	}
	return spec
}

// buildinTemplateDir returns the absolute path to the on-disk
// `tasks/buildin/template/` task that ships with dicode-core. Anchored
// via runtime.Caller so it works regardless of the test runner's CWD
// (go test sets CWD to the package dir, but worktree-relative anchors
// drift if the layout ever changes). The walk-up is fixed: this file
// lives at `pkg/trigger/`, two levels under the repo root, so the
// buildin tasks dir is at `../../tasks/buildin/template`.
func buildinTemplateDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot anchor buildin/template path")
	}
	// thisFile is .../pkg/trigger/e2e_input_output_test.go
	// → repo root is two levels up from the file's directory.
	pkgDir := filepath.Dir(thisFile)               // .../pkg/trigger
	repoRoot := filepath.Dir(filepath.Dir(pkgDir)) // .../
	dir := filepath.Join(repoRoot, "tasks", "buildin", "template")
	if _, err := os.Stat(filepath.Join(dir, "task.yaml")); err != nil {
		t.Fatalf("buildin/template task.yaml not found at %s: %v", dir, err)
	}
	return dir
}

// loadBuildinTemplateAs loads the real `tasks/buildin/template/` task
// from disk and rebinds its ID + Name so the test can wire a downstream
// chain trigger that references this upstream by a stable, test-scoped
// name (LoadDir defaults the ID to filepath.Base of the dir, which
// would collide if the same suite registered multiple buildin/template
// instances across subtests).
//
// envAllow appends entries to permissions.env so the template body's
// `${VAR}` placeholders can be resolved without enabling unrestricted
// --allow-env. Each entry's Value is taken from the current process
// env at registration time — t.Setenv() in the caller is the
// recommended source.
//
// We re-validate after rebinding to surface any spec-level regressions
// that would otherwise only fire once the engine dispatched the task.
func loadBuildinTemplateAs(t *testing.T, id string, envAllow []task.EnvEntry) *task.Spec {
	t.Helper()
	spec, err := task.LoadDir(buildinTemplateDir(t))
	if err != nil {
		t.Fatalf("LoadDir buildin/template: %v", err)
	}
	spec.ID = id
	spec.Name = id
	if len(envAllow) > 0 {
		spec.Permissions.Env = append(spec.Permissions.Env, envAllow...)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("buildin/template re-validate after rebind: %v", err)
	}
	return spec
}

// TestE2E_InputOutput_ChainParamsStringSubstitution drives the
// happy-path success-chain dispatch through real Deno: the REAL
// `tasks/buildin/template/` task loaded from disk renders a literal
// string ("hello from upstream" — no placeholders, so no env wiring
// needed) and feeds a downstream whose `chain.params.greeting` is the
// literal `${input.output}` token. The downstream's task body reads
// `input.greeting` and writes it to a marker file, proving the
// resolver substituted the token BEFORE the engine packaged the input
// for delivery.
//
// Using the real buildin/template task exercises the production code
// path (its `run_result.enabled: false` + in-memory cache return) that
// PR #310 was designed for. If the engine accidentally routed chain
// delivery through the persisted `runs.return_value` column (which
// buildin/template suppresses), the marker file would never appear.
func TestE2E_InputOutput_ChainParamsStringSubstitution(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	dir := t.TempDir()
	markerDir := t.TempDir()
	markerPath := filepath.Join(markerDir, "greeting.marker")
	t.Setenv("MARKER_PATH", markerPath)

	e := newTestEnv(t)

	const upstreamID = "upstream-string"
	const downstreamID = "downstream-marker"
	// No ${VAR} placeholders → buildin/template renders this verbatim
	// and the downstream sees it as input.greeting after token
	// substitution. Keeps the env-permission surface empty.
	const upstreamReturn = "hello from upstream"

	// Downstream — chained from the upstream with the literal token
	// in chain.params.greeting. Reads input.greeting (the engine's
	// merged ChainInput map) and writes it to disk so the test can
	// observe the resolved value reaching a real task body.
	downstream := writeMarkerTask(t, dir, downstreamID, `
export default async function main({ input }: any) {
  const path = Deno.env.get("MARKER_PATH");
  if (!path) throw new Error("MARKER_PATH not set");
  const g = input.greeting;
  if (typeof g !== "string") {
    throw new Error("greeting is not a string; input=" + JSON.stringify(input));
  }
  await Deno.writeTextFile(path, g);
}
`, markerDir, task.TriggerConfig{
		Chain: &task.ChainTrigger{
			From:   upstreamID,
			On:     "success",
			Params: map[string]any{"greeting": "${input.output}"},
		},
	}, false)
	if err := e.reg.Register(downstream); err != nil {
		t.Fatalf("reg.Register downstream: %v", err)
	}

	// Upstream — REAL buildin/template loaded from disk. No env entries
	// needed because the template body is a plain literal. The
	// suppressed `run_result.enabled: false` in the on-disk YAML routes
	// the rendered string through the in-memory cache exactly as the
	// shipped buildin/template task does in production.
	upstream := loadBuildinTemplateAs(t, upstreamID, nil)
	if err := e.reg.Register(upstream); err != nil {
		t.Fatalf("reg.Register upstream: %v", err)
	}

	upRunID, err := e.engine.FireManual(context.Background(), upstreamID,
		map[string]string{"template": upstreamReturn})
	if err != nil {
		t.Fatalf("FireManual upstream: %v", err)
	}
	primary := waitForTerminal(t, e.engine, upRunID, 30*time.Second)
	if primary.Status != registry.StatusSuccess {
		t.Fatalf("upstream status = %q, want success", primary.Status)
	}

	// Wait for the chained downstream to complete.
	got := waitForRunOfTask(t, e.engine, downstreamID, 30*time.Second)
	if got == nil {
		t.Fatal("downstream was not fired within the timeout")
	}
	if got.Status != registry.StatusSuccess {
		t.Errorf("downstream status = %q, want success", got.Status)
	}

	// The on-disk marker is the load-bearing assertion: the resolved
	// upstream-return string reached the downstream task body, NOT the
	// literal `${input.output}` token.
	markerContent := pollFileContents(t, markerPath, 5*time.Second)
	if markerContent != upstreamReturn {
		t.Errorf("marker file = %q, want %q (resolver did not substitute the token)",
			markerContent, upstreamReturn)
	}
}

// TestE2E_InputOutput_NonStringUpstreamFailsLoudly verifies the
// resolver short-circuits when the upstream's real Deno return value
// is non-string (here, a JSON object). The downstream task must NOT
// run — its body would write a marker file if it did. The absence of
// the marker, plus the registry showing zero runs of the downstream,
// proves the resolver caught the type mismatch at dispatch.
//
// Why this case uses an INLINE upstream rather than buildin/template:
// buildin/template's contract is "render a string from a template",
// so it can never return a non-string. The negative-case fixture
// needs a task that actually returns an object — a separate inline
// Deno script is the minimal way to drive that.
//
// This is the e2e companion to
// TestOnFailureChainDispatch_NonStringUpstreamSkips (which drives the
// failure-chain edge via direct FireChain calls); here we exercise
// the success-chain edge with a real Deno return-value pipeline so a
// bug in the JSON marshalling / runReturnValue cache path that only
// surfaces end-to-end would still be caught.
func TestE2E_InputOutput_NonStringUpstreamFailsLoudly(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	dir := t.TempDir()
	markerDir := t.TempDir()
	markerPath := filepath.Join(markerDir, "didrun.marker")
	t.Setenv("MARKER_PATH", markerPath)

	e := newTestEnv(t)

	const upstreamID = "upstream-object"
	const downstreamID = "downstream-skipped"

	// Downstream — would write a "did-run" marker if it actually
	// dispatched. The resolver must short-circuit before this body
	// ever executes.
	downstream := writeMarkerTask(t, dir, downstreamID, `
export default async function main() {
  const path = Deno.env.get("MARKER_PATH");
  if (path) await Deno.writeTextFile(path, "ran");
}
`, markerDir, task.TriggerConfig{
		Chain: &task.ChainTrigger{
			From:   upstreamID,
			On:     "success",
			Params: map[string]any{"x": "${input.output}"},
		},
	}, false)
	if err := e.reg.Register(downstream); err != nil {
		t.Fatalf("reg.Register downstream: %v", err)
	}

	// Upstream returns a JSON OBJECT — stringRet's contract turns
	// every non-string return into "", which propagates as
	// ErrInputUnavailable through ResolveInputOutputMap and causes
	// the chain dispatch to skip with an error log. Inline rather
	// than buildin/template because buildin/template only ever
	// returns strings.
	upstream := writeMarkerTask(t, dir, upstreamID,
		`export default async function main() { return { key: "value" } }`,
		markerDir,
		task.TriggerConfig{Manual: true},
		false,
	)
	if err := e.reg.Register(upstream); err != nil {
		t.Fatalf("reg.Register upstream: %v", err)
	}

	upRunID, err := e.engine.FireManual(context.Background(), upstreamID, nil)
	if err != nil {
		t.Fatalf("FireManual upstream: %v", err)
	}
	primary := waitForTerminal(t, e.engine, upRunID, 30*time.Second)
	if primary.Status != registry.StatusSuccess {
		t.Fatalf("upstream status = %q, want success", primary.Status)
	}

	// Give the chain dispatcher a window to (incorrectly) fire.
	// Mirrors the cushion used by
	// TestOnFailureChainDispatch_NonStringUpstreamSkips — chain
	// dispatch is goroutine-driven so a "didn't fire" assertion needs
	// a sleep, not just an immediate poll.
	time.Sleep(2 * time.Second)

	// Registry assertion: the downstream must have zero runs. If the
	// resolver passed the literal token through, fireAsync would have
	// landed a downstream row.
	runs, err := e.engine.registry.ListRuns(context.Background(), downstreamID, 5)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) > 0 {
		t.Errorf("downstream %s should not have run when ${input.output} cannot resolve; got %d runs",
			downstreamID, len(runs))
	}

	// Filesystem assertion: belt + braces. Even if a future refactor
	// stops creating run rows for short-circuited dispatches, the
	// task body never running is the user-observable contract.
	if _, err := os.Stat(markerPath); err == nil {
		b, _ := os.ReadFile(markerPath)
		t.Errorf("marker file should not exist; got contents %q", string(b))
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected stat error on marker file: %v", err)
	}
}

// TestE2E_InputOutput_EmbeddedTokenPassesThroughLiterally pins the
// narrow grammar of the resolver: only param values whose VALUE IS
// EXACTLY `${input.output}` are interpolated. An embedded reference
// like `"prefix-${input.output}-suffix"` must reach the downstream
// as a literal string — no partial substitution, no error.
//
// The hand-written tests in pkg/task/inputref_test.go assert this at
// the unit layer; this e2e pins it at the chain-dispatch boundary so
// a future refactor that swaps in a more lenient regex-based resolver
// can't accidentally pass through. Upstream is the REAL
// `tasks/buildin/template/` task — the rendered string is what
// flows into `input.output` via the in-memory cache.
func TestE2E_InputOutput_EmbeddedTokenPassesThroughLiterally(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	dir := t.TempDir()
	markerDir := t.TempDir()
	markerPath := filepath.Join(markerDir, "embedded.marker")
	t.Setenv("MARKER_PATH", markerPath)

	e := newTestEnv(t)

	const upstreamID = "upstream-embed"
	const downstreamID = "downstream-embed"
	const embeddedLiteral = "prefix-${input.output}-suffix"

	downstream := writeMarkerTask(t, dir, downstreamID, `
export default async function main({ input }: any) {
  const path = Deno.env.get("MARKER_PATH");
  if (!path) throw new Error("MARKER_PATH not set");
  const v = input.embedded;
  if (typeof v !== "string") {
    throw new Error("embedded is not a string; input=" + JSON.stringify(input));
  }
  await Deno.writeTextFile(path, v);
}
`, markerDir, task.TriggerConfig{
		Chain: &task.ChainTrigger{
			From:   upstreamID,
			On:     "success",
			Params: map[string]any{"embedded": embeddedLiteral},
		},
	}, false)
	if err := e.reg.Register(downstream); err != nil {
		t.Fatalf("reg.Register downstream: %v", err)
	}

	upstream := loadBuildinTemplateAs(t, upstreamID, nil)
	if err := e.reg.Register(upstream); err != nil {
		t.Fatalf("reg.Register upstream: %v", err)
	}

	upRunID, err := e.engine.FireManual(context.Background(), upstreamID,
		map[string]string{"template": "the-real-value"})
	if err != nil {
		t.Fatalf("FireManual upstream: %v", err)
	}
	primary := waitForTerminal(t, e.engine, upRunID, 30*time.Second)
	if primary.Status != registry.StatusSuccess {
		t.Fatalf("upstream status = %q, want success", primary.Status)
	}

	got := waitForRunOfTask(t, e.engine, downstreamID, 30*time.Second)
	if got == nil {
		t.Fatal("downstream was not fired within the timeout")
	}
	if got.Status != registry.StatusSuccess {
		t.Errorf("downstream status = %q, want success", got.Status)
	}

	// The marker MUST hold the literal string verbatim — partial
	// substitution like "prefix-the-real-value-suffix" would mean the
	// resolver had moved beyond the narrow "value-is-exactly-token"
	// grammar PR #310 ships.
	markerContent := pollFileContents(t, markerPath, 5*time.Second)
	if markerContent != embeddedLiteral {
		t.Errorf("marker file = %q, want literal %q (resolver should not interpolate embedded tokens)",
			markerContent, embeddedLiteral)
	}
}

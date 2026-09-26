package approval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

// pendSpec admits spec into a gate that holds it, returning the gate and the
// state the operator would review.
func pendSpec(t *testing.T, spec task.Kinded) (*Gate, State) {
	t.Helper()
	g, _, _ := newTestGate(t, enabledPolicy())
	armed, err := g.Admit(spec)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if armed {
		t.Fatal("task must be pending for a review state to exist")
	}
	st, err := g.State(spec.TaskID())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	return g, st
}

func TestStateRendersResolvedTask(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	spec.Name = "Deploy"
	spec.Description = "ships it"
	spec.Runtime = task.Runtime("deno")
	spec.Timeout = 90 * time.Second
	spec.Trigger = task.TriggerConfig{Cron: "0 9 * * *"}
	spec.Permissions = task.Permissions{
		Net: []string{"api.github.com"},
		Run: []string{"git"},
		FS:  []task.FSEntry{{Path: "/tmp/out", Permission: "rw"}},
	}

	_, st := pendSpec(t, spec)

	if st.TaskID != "repo/deploy" {
		t.Errorf("task id: got %q", st.TaskID)
	}
	if st.Runtime != "deno" {
		t.Errorf("runtime: got %q", st.Runtime)
	}
	if st.Timeout != "1m30s" {
		t.Errorf("timeout: got %q", st.Timeout)
	}
	if len(st.Triggers) != 1 || st.Triggers[0].Kind != TriggerCron || st.Triggers[0].Cron != "0 9 * * *" {
		t.Errorf("triggers: got %+v", st.Triggers)
	}
	if len(st.Permissions.Net) != 1 || st.Permissions.Net[0] != "api.github.com" {
		t.Errorf("net: got %+v", st.Permissions.Net)
	}
	if len(st.Permissions.FS) != 1 || st.Permissions.FS[0].Path != "/tmp/out" {
		t.Errorf("fs: got %+v", st.Permissions.FS)
	}
}

// The operator approves the version they reviewed (ApproveIfHash), so the
// state must carry the hash it was rendered at.
func TestStateCarriesPendingHash(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	g, st := pendSpec(t, spec)

	want, ok := g.PendingHash("repo/deploy")
	if !ok {
		t.Fatal("task not pending")
	}
	if st.PendingHash != want {
		t.Errorf("pending hash: want %q, got %q", want, st.PendingHash)
	}
	if err := g.ApproveIfHash("repo/deploy", st.PendingHash); err != nil {
		t.Fatalf("the reviewed hash must approve: %v", err)
	}
}

// ADR-0003: an env entry renders as its declaration; the reference is never
// followed and a literal value never reaches the surface.
func TestStateRendersEnvDeclarationsWithoutValues(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	spec.Permissions = task.Permissions{Env: []task.EnvEntry{
		{Name: "API_KEY", From: "env:GH_TOKEN"},
		{Name: "DB_PASS", Secret: "db_password", Default: "hunter2"},
		{Name: "PG_URL", From: "task:doppler"},
		{Name: "LOG_LEVEL", Value: "s3cr3t-literal"},
		{Name: "HOME"},
	}}

	_, st := pendSpec(t, spec)

	if len(st.Env) != 5 {
		t.Fatalf("want 5 env declarations, got %d: %+v", len(st.Env), st.Env)
	}
	byName := map[string]EnvDecl{}
	for _, e := range st.Env {
		byName[e.Name] = e
	}
	if got := byName["API_KEY"]; got.Kind != EnvFromHost || got.Ref != "GH_TOKEN" {
		t.Errorf("API_KEY: got %+v", got)
	}
	if got := byName["DB_PASS"]; got.Kind != EnvFromSecret || got.Ref != "db_password" || !got.HasDefault {
		t.Errorf("DB_PASS: got %+v", got)
	}
	if got := byName["PG_URL"]; got.Kind != EnvFromTask || got.Ref != "doppler" {
		t.Errorf("PG_URL: got %+v", got)
	}
	if got := byName["LOG_LEVEL"]; got.Kind != EnvLiteral || got.Ref != "" {
		t.Errorf("LOG_LEVEL: got %+v", got)
	}
	if got := byName["HOME"]; got.Kind != EnvFromHost || got.Ref != "HOME" {
		t.Errorf("HOME: got %+v", got)
	}

	// The whole surface, serialised as the API ships it, holds no value.
	blob := mustJSON(t, st)
	for _, leak := range []string{"hunter2", "s3cr3t-literal"} {
		if strings.Contains(blob, leak) {
			t.Errorf("literal %q reached the review surface: %s", leak, blob)
		}
	}
}

// A webhook secret is never rendered; that one is configured at all is.
func TestStateNeverRendersWebhookSecret(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	spec.Trigger = task.TriggerConfig{Webhook: "/hooks/deploy", WebhookSecret: "top-secret-hmac"}

	_, st := pendSpec(t, spec)

	if len(st.Triggers) != 1 {
		t.Fatalf("want 1 trigger, got %+v", st.Triggers)
	}
	tr := st.Triggers[0]
	if tr.Kind != TriggerWebhook || tr.Webhook != "/hooks/deploy" {
		t.Errorf("trigger: got %+v", tr)
	}
	if !tr.Signed {
		t.Error("a configured webhook secret must show as signed")
	}
	if blob := mustJSON(t, st); strings.Contains(blob, "top-secret-hmac") {
		t.Errorf("webhook secret reached the review surface: %s", blob)
	}
}

// A param default is author-written literal content and does not render
// (ADR-0003).
func TestStateRendersParamsWithoutDefaults(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	spec.Params = task.Params{
		{Name: "repo", Type: "string", Default: "dicode-ayo/dicode-core", Description: "target"},
		{Name: "token", Default: "leaked-credential"},
		{Name: "limit", Required: true},
	}

	_, st := pendSpec(t, spec)

	if len(st.Params) != 3 {
		t.Fatalf("want 3 params, got %+v", st.Params)
	}
	if !st.Params[0].HasDefault || st.Params[0].Name != "repo" || st.Params[0].Type != "string" {
		t.Errorf("repo: got %+v", st.Params[0])
	}
	if st.Params[2].HasDefault || !st.Params[2].Required {
		t.Errorf("limit: got %+v", st.Params[2])
	}
	if blob := mustJSON(t, st); strings.Contains(blob, "leaked-credential") {
		t.Errorf("param default reached the review surface: %s", blob)
	}
}

// The inventory is the one code-shaped fact the spec cannot carry.
func TestStateListsFileInventory(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	_, st := pendSpec(t, spec)

	paths := map[string]bool{}
	for _, f := range st.Files {
		paths[f.Path] = true
		if f.Kind == task.FileKindRegular && f.Hash == "" {
			t.Errorf("%s: regular file with no hash", f.Path)
		}
	}
	for _, want := range []string{"task.yaml", "task.js"} {
		if !paths[want] {
			t.Errorf("inventory missing %s: %+v", want, st.Files)
		}
	}
}

// No file content may render, whatever a file happens to contain.
func TestStateInventoryCarriesNoFileContent(t *testing.T) {
	root := t.TempDir()
	spec := writeTaskDir(t, root, "repo/deploy", "const secret = 'file-body-marker';")
	_, st := pendSpec(t, spec)

	if blob := mustJSON(t, st); strings.Contains(blob, "file-body-marker") {
		t.Errorf("file content reached the review surface: %s", blob)
	}
}

// A hash_include target re-pends the task from outside its directory, so it
// must be visible in the inventory.
func TestStateInventoryCoversHashInclude(t *testing.T) {
	root := t.TempDir()
	spec := writeTaskDir(t, root, "repo/deploy", "export default () => {}")
	shared := filepath.Join(root, "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "lib.js"), []byte("export const v = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec.HashInclude = []string{"../shared/lib.js"}

	_, st := pendSpec(t, spec)

	for _, f := range st.Files {
		if f.Path == "include:../shared/lib.js" {
			return
		}
	}
	t.Errorf("hash_include target missing from inventory: %+v", st.Files)
}

// End state renders from the checkout, so a task with no git history, no
// baseline and no prior approval still gets a complete surface.
func TestStateNeedsNoBaseline(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/fresh", "export default () => {}")
	_, st := pendSpec(t, spec)

	if len(st.Files) == 0 {
		t.Errorf("first-ever observation must still inventory its files: %+v", st)
	}
	if st.PendingHash == "" {
		t.Error("pending hash missing")
	}
	if st.Kind != task.KindTask {
		t.Errorf("kind: got %q", st.Kind)
	}
}

// TestStateAppliesPreviewFn is the code-review regression for #832's
// pending-review surface: a daemon-config-driven override that only used to
// apply at arm time (once a task was already approved) must also be visible
// on the pending-review surface for a task that can now pend before ever
// being armed — otherwise the surface an operator approves from doesn't
// match the end state Approve actually produces.
func TestStateAppliesPreviewFn(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	g.SetPreviewFn(func(k task.Kinded) task.Kinded {
		s, ok := k.(*task.Spec)
		if !ok || s.ID != "repo/preview-me" {
			return k
		}
		override := *s
		override.Enabled = false
		return &override
	})

	spec := writeTaskDir(t, t.TempDir(), "repo/preview-me", "export default () => {}")
	spec.Enabled = true
	armed, err := g.Admit(spec)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if armed {
		t.Fatal("task must be pending for a review state to exist")
	}

	st, err := g.State("repo/preview-me")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Enabled {
		t.Error("State must render previewFn's transformed value (Enabled: false), not ent.kinded's raw Enabled: true")
	}
	if spec.Enabled != true {
		t.Error("previewFn must not mutate the pending entry's underlying spec")
	}
}

// TestStateNilPreviewFnIsIdentity guards the common case (no daemon wired a
// preview transform, e.g. every existing test in this package): State must
// behave exactly as before when previewFn is nil.
func TestStateNilPreviewFnIsIdentity(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/no-preview", "export default () => {}")
	spec.Enabled = true
	_, st := pendSpec(t, spec)

	if !st.Enabled {
		t.Error("with no previewFn installed, State must render the pending entry's own Enabled value")
	}
}

func TestStateErrorsOnNonPendingTask(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	if _, err := g.State("repo/deploy"); err == nil {
		t.Fatal("want an error for an unknown task")
	}
	if _, err := g.Admit(spec); err != nil {
		t.Fatal(err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.State("repo/deploy"); err == nil {
		t.Fatal("want an error once the task is no longer pending")
	}
}

// Taskset overrides feed the content hash from outside the repository, so the
// state must show the resolved values rather than the file's own.
func TestStateShowsResolvedOverriddenPermissions(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	spec.Permissions = task.Permissions{Net: []string{"evil.example.com"}}
	spec.Runtime = task.Runtime("python")

	_, st := pendSpec(t, spec)

	if st.Runtime != "python" {
		t.Errorf("runtime: got %q", st.Runtime)
	}
	if len(st.Permissions.Net) != 1 || st.Permissions.Net[0] != "evil.example.com" {
		t.Errorf("net: got %+v", st.Permissions.Net)
	}
}

func TestStateRendersPipelineStages(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pipe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: pipe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &task.PipelineTask{
		ID: "repo/pipe", TaskDir: dir, Kind: task.KindPipelineTask,
		Subtype: "sequential",
		Trigger: task.PipelineTrigger{Manual: true},
		Stages:  []task.Stage{{Task: "repo/build"}, {Task: "repo/deploy"}},
	}

	_, st := pendSpec(t, p)

	if st.Kind != task.KindPipelineTask {
		t.Errorf("kind: got %q", st.Kind)
	}
	if len(st.Stages) != 2 || st.Stages[0].Task != "repo/build" {
		t.Errorf("stages: got %+v", st.Stages)
	}
	// A pipeline has no runtime or permissions of its own.
	if st.Runtime != "" {
		t.Errorf("pipeline must have no runtime, got %q", st.Runtime)
	}
	if len(st.Triggers) != 1 || st.Triggers[0].Kind != TriggerManual {
		t.Errorf("triggers: got %+v", st.Triggers)
	}
}

func TestStateRendersContainerImage(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/box", "")
	spec.Runtime = task.Runtime("docker")
	spec.Docker = &task.DockerConfig{Image: "alpine:3.20", NetworkMode: "host"}

	_, st := pendSpec(t, spec)

	if st.Image != "alpine:3.20" {
		t.Errorf("image: got %q", st.Image)
	}
	if st.NetworkMode != "host" {
		t.Errorf("network mode: got %q", st.NetworkMode)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// An unresolved ${VAR} placeholder is not a secret. Under auth: session it
// survives normalization (only auth: any clears it), so reporting it as signed
// would claim a verification the webhook cannot perform.
func TestStateDoesNotCallAnUnresolvedPlaceholderSigned(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	spec.Trigger = task.TriggerConfig{
		Webhook:       "/hooks/deploy",
		WebhookAuth:   task.WebhookAuthSession,
		WebhookSecret: "${DEPLOY_HMAC}",
	}

	_, st := pendSpec(t, spec)

	if len(st.Triggers) != 1 {
		t.Fatalf("want 1 trigger, got %+v", st.Triggers)
	}
	if st.Triggers[0].Signed {
		t.Error("an unresolved placeholder must not render as a signed webhook")
	}
	if blob := mustJSON(t, st); strings.Contains(blob, "DEPLOY_HMAC") {
		t.Errorf("the secret reference reached the review surface: %s", blob)
	}
}

// A dir-less task legitimately has no files, so an empty list alone cannot
// distinguish "nothing to list" from "the listing failed".
func TestStateReportsAnInventoryFailure(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	// Escapes the sibling-task boundary, so Inventory refuses to walk it.
	spec.HashInclude = []string{"../../../../etc/passwd"}

	_, st := pendSpec(t, spec)

	if st.FilesError == "" {
		t.Error("a failed inventory must be reported on the surface, not only logged")
	}
}

// The dashboard consumes this surface as JSON, so the contract is the wire
// format, not the Go struct. Asserting on fields alone misses a type whose
// tags marshal by Go name — which renders as a blank row for a grant the
// operator is being asked to arm.
func TestStateWireFormatMatchesWhatTheRendererReads(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	spec.Runtime = task.Runtime("deno")
	spec.Trigger = task.TriggerConfig{Cron: "0 9 * * *"}
	spec.Params = task.Params{{Name: "repo", Default: "x"}}
	spec.Permissions = task.Permissions{
		Net: []string{"api.github.com"},
		FS:  []task.FSEntry{{Path: "/etc", Permission: "rw"}},
		Env: []task.EnvEntry{{Name: "API_KEY", From: "env:GH_TOKEN"}},
	}
	spec.Docker = &task.DockerConfig{
		Image: "alpine:3.20", Volumes: []string{"/:/host"}, CapAdd: []string{"SYS_ADMIN"},
		CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
		User: "root", ReadOnly: true, EnvVars: map[string]string{"TOKEN": "leak"},
	}

	_, st := pendSpec(t, spec)

	var wire map[string]any
	if err := json.Unmarshal([]byte(mustJSON(t, st)), &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	perms, ok := wire["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions missing from the wire form: %v", wire)
	}
	fs, ok := perms["fs"].([]any)
	if !ok || len(fs) != 1 {
		t.Fatalf("permissions.fs: got %v", perms["fs"])
	}
	entry, _ := fs[0].(map[string]any)
	if entry["path"] != "/etc" {
		t.Errorf(`permissions.fs[0].path: want "/etc", got %v (keys: %v)`, entry["path"], keysOf(entry))
	}
	if entry["permission"] != "rw" {
		t.Errorf(`permissions.fs[0].permission: want "rw", got %v (keys: %v)`, entry["permission"], keysOf(entry))
	}

	// The rest of the keys dc-task-detail.js reads off the payload.
	for _, path := range [][2]string{
		{"task_id", ""}, {"pending_hash", ""}, {"runtime", ""},
	} {
		if _, ok := wire[path[0]]; !ok {
			t.Errorf("wire form missing %q: keys %v", path[0], keysOf(wire))
		}
	}
	files, _ := wire["files"].([]any)
	if len(files) == 0 {
		t.Fatal("files missing from the wire form")
	}
	f, _ := files[0].(map[string]any)
	for _, k := range []string{"path", "kind", "hash"} {
		if _, ok := f[k]; !ok {
			t.Errorf("file entry missing %q: keys %v", k, keysOf(f))
		}
	}
	env, _ := wire["env"].([]any)
	e, _ := env[0].(map[string]any)
	for _, k := range []string{"name", "kind", "ref"} {
		if _, ok := e[k]; !ok {
			t.Errorf("env entry missing %q: keys %v", k, keysOf(e))
		}
	}
	params, _ := wire["params"].([]any)
	pm, _ := params[0].(map[string]any)
	if _, ok := pm["has_default"]; !ok {
		t.Errorf("param entry missing %q: keys %v", "has_default", keysOf(pm))
	}
	// Container hardening decides what the task can reach independently of
	// permissions, so every field of it is part of the contract.
	if c, ok := wire["container"].(map[string]any); ok {
		for _, k := range []string{"volumes", "cap_add", "cap_drop", "security_opt", "user", "read_only", "env_names"} {
			if _, ok := c[k]; !ok {
				t.Errorf("container missing %q: keys %v", k, keysOf(c))
			}
		}
	}

	trig, _ := wire["triggers"].([]any)
	tr, _ := trig[0].(map[string]any)
	for _, k := range []string{"kind", "cron"} {
		if _, ok := tr[k]; !ok {
			t.Errorf("trigger entry missing %q: keys %v", k, keysOf(tr))
		}
	}
}

// ── CurrentState (#714) ──────────────────────────────────────────────────────
//
// State is scoped to the pend/approve window: it 409s the moment a task
// stops being pending, leaving no way to answer "what can this armed task
// reach?" the rest of the time. CurrentState is the counterpart for that
// case — these tests pin its contract against State's.

// TestCurrentState_RendersArmedTaskWithEmptyPendingHash is the core
// regression for #714: an armed (no-longer-pending) task's resolved
// permissions/triggers must still be readable, and the render must never
// carry a hash that looks like a usable pending approval.
func TestCurrentState_RendersArmedTaskWithEmptyPendingHash(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	spec.Permissions = task.Permissions{Net: []string{"api.github.com"}}
	spec.Trigger = task.TriggerConfig{Cron: "0 9 * * *"}

	if _, err := g.Admit(spec); err != nil {
		t.Fatal(err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatal(err)
	}
	// Once approved the task is no longer pending: State must refuse it —
	// CurrentState is the only path left for reading its resolved state.
	if _, err := g.State("repo/deploy"); err == nil {
		t.Fatal("State must still error once the task is armed")
	}

	st := g.CurrentState("repo/deploy", spec)
	if st.PendingHash != "" {
		t.Errorf("PendingHash = %q, want empty — an armed task has no in-flight approval to bind to", st.PendingHash)
	}
	if st.TaskID != "repo/deploy" {
		t.Errorf("TaskID = %q", st.TaskID)
	}
	if len(st.Permissions.Net) != 1 || st.Permissions.Net[0] != "api.github.com" {
		t.Errorf("Permissions.Net = %+v", st.Permissions.Net)
	}
	if len(st.Triggers) != 1 || st.Triggers[0].Cron != "0 9 * * *" {
		t.Errorf("Triggers = %+v", st.Triggers)
	}
}

// TestCurrentState_AppliesPreviewFn mirrors TestStateAppliesPreviewFnOverride:
// an armed buildin task with a daemon-config override (e.g. relay-server's
// Enabled toggle) must render the overridden value here too, not the
// as-shipped one — the same reasoning State already applies previewFn for.
func TestCurrentState_AppliesPreviewFn(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	g.SetPreviewFn(func(k task.Kinded) task.Kinded {
		s, ok := k.(*task.Spec)
		if !ok || s.ID != "repo/preview-me" {
			return k
		}
		override := *s
		override.Enabled = false
		return &override
	})

	spec := writeTaskDir(t, t.TempDir(), "repo/preview-me", "export default () => {}")
	spec.Enabled = true

	st := g.CurrentState("repo/preview-me", spec)
	if st.Enabled {
		t.Error("CurrentState must render previewFn's transformed value (Enabled: false), not the raw spec's Enabled: true")
	}
	if spec.Enabled != true {
		t.Error("previewFn must not mutate the caller's spec")
	}
}

// TestCurrentState_MatchesStateExceptPendingHash pins the contract from
// #714: for the same underlying spec, CurrentState and State must render
// identically except for PendingHash, so a client reading either endpoint
// sees the same "what will run" answer.
func TestCurrentState_MatchesStateExceptPendingHash(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	spec.Permissions = task.Permissions{Net: []string{"api.github.com"}, Run: []string{"git"}}
	spec.Params = task.Params{{Name: "env", Required: true}}

	g, pendingState := pendSpec(t, spec)
	currentState := g.CurrentState("repo/deploy", spec)

	pendingState.PendingHash = ""
	if !reflect.DeepEqual(pendingState, currentState) {
		t.Errorf("CurrentState diverges from State (modulo PendingHash):\nState:        %+v\nCurrentState: %+v", pendingState, currentState)
	}
}

// ── StateFor (code-review follow-up on #714's PR) ────────────────────────────
//
// apiTaskState originally checked IsPending and then separately called State
// or CurrentState — two locked reads of the pending map instead of one,
// leaving a window where a task transitioning from not-pending to pending
// in between would render CurrentState's empty PendingHash for a task that,
// by the time the caller acts on the response, genuinely is pending.
// StateFor closes that by deciding pending-vs-not and reading the entry it
// renders from together, under one lock acquisition.

// TestStateFor_PendingTaskReturnsPendingHash pins the pending branch: a
// pending task's StateFor render must carry the real, non-empty hash that
// ApproveIfHash would check — the same value State(id) would return.
func TestStateFor_PendingTaskReturnsPendingHash(t *testing.T) {
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	g, pendingState := pendSpec(t, spec)

	st := g.StateFor("repo/deploy", spec)
	if st.PendingHash == "" {
		t.Fatal("StateFor: PendingHash empty for a genuinely pending task")
	}
	if !reflect.DeepEqual(pendingState, st) {
		t.Errorf("StateFor diverges from State for a pending task:\nState:    %+v\nStateFor: %+v", pendingState, st)
	}
}

// TestStateFor_ArmedTaskReturnsEmptyPendingHash pins the non-pending branch:
// an armed task's StateFor render must carry an empty PendingHash, exactly
// like CurrentState.
func TestStateFor_ArmedTaskReturnsEmptyPendingHash(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	if _, err := g.Admit(spec); err != nil {
		t.Fatal(err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatal(err)
	}

	st := g.StateFor("repo/deploy", spec)
	if st.PendingHash != "" {
		t.Errorf("PendingHash = %q, want empty for an armed task", st.PendingHash)
	}
}

// TestStateFor_ConcurrentWithAdmit races StateFor against Admit on the same
// id under the race detector (`go test -race`, as CI runs) — the exact
// scenario CodeRabbit's review of #858 flagged: a task transitioning from
// not-pending to pending while a state read is in flight. StateFor's single
// locked snapshot of the pending map means every observed result is
// internally consistent (a real hash iff the entry was pending at that
// snapshot); this test's job is to prove there's no data race in reaching
// that snapshot, and spot-check the consistency invariant along the way.
func TestStateFor_ConcurrentWithAdmit(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/race", "export default () => {}")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			st := g.StateFor("repo/race", spec)
			// Consistency invariant: StateFor never fabricates a hash for an
			// id it renders as the caller-supplied spec (TaskID always
			// matches; a non-empty hash only ever comes from a real pending
			// entry, per renderState's construction).
			if st.TaskID != "repo/race" {
				t.Errorf("TaskID = %q, want repo/race", st.TaskID)
			}
		}
	}()

	if _, err := g.Admit(spec); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()

	// Once Admit has returned, the task is definitely pending: StateFor must
	// now consistently report a real hash.
	if st := g.StateFor("repo/race", spec); st.PendingHash == "" {
		t.Error("StateFor after Admit: PendingHash empty for a pending task")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

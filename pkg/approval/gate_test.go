package approval

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// fakeArm records every task it was asked to arm.
type fakeArm struct {
	mu     sync.Mutex
	armed  []string
	armErr error
}

func (f *fakeArm) arm(k task.Kinded) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.armErr != nil {
		return f.armErr
	}
	f.armed = append(f.armed, k.TaskID())
	return nil
}

func (f *fakeArm) armedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.armed...)
}

// writeTaskDir creates a real task directory so ContentHash exercises
// task.Hash, and returns a spec pointing at it.
func writeTaskDir(t *testing.T, root, id, script string) *task.Spec {
	t.Helper()
	dir := filepath.Join(root, strings.ReplaceAll(id, "/", "__"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: "+id+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.js"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	return &task.Spec{ID: id, TaskDir: dir}
}

func newTestGate(t *testing.T, policy Policy) (*Gate, *fakeArm, *Lock) {
	t.Helper()
	lock, err := LoadLock(filepath.Join(t.TempDir(), LockFileName))
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	arm := &fakeArm{}
	return NewGate(policy, lock, "", arm.arm, nil), arm, lock
}

func enabledPolicy() Policy {
	return Policy{Enabled: true, TrustedSources: map[string]bool{}, TrustedTasks: map[string]bool{}}
}

func TestNewUntrustedTaskPending(t *testing.T) {
	g, arm, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")

	armed, err := g.Admit(spec)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if armed {
		t.Fatal("new untrusted task must be pending, got armed")
	}
	if got := arm.armedIDs(); len(got) != 0 {
		t.Fatalf("arm called for pending task: %v", got)
	}
	if got := g.Pending(); len(got) != 1 || got[0] != "repo/deploy" {
		t.Fatalf("Pending() = %v", got)
	}
	if _, ok := lock.Get("repo/deploy"); ok {
		t.Fatal("pending task must not get a lock record")
	}
	if err := g.FireGuard("repo/deploy"); !errors.Is(err, ErrPending) {
		t.Fatalf("FireGuard = %v, want ErrPending", err)
	}
}

func TestBootstrapSeedsThenGates(t *testing.T) {
	g, arm, lock := newTestGate(t, enabledPolicy())
	g.SetBootstrap(true)

	// During bootstrap an untrusted task is seeded (armed + recorded), not held.
	existing := writeTaskDir(t, t.TempDir(), "repo/existing", "x")
	armed, err := g.Admit(existing)
	if err != nil || !armed {
		t.Fatalf("bootstrap Admit = (%v, %v), want (true, nil)", armed, err)
	}
	rec, ok := lock.Get("repo/existing")
	if !ok || rec.ApprovedBy != ApprovedByBootstrap {
		t.Fatalf("bootstrap task lock record = %+v (ok=%v), want approved_by=%q", rec, ok, ApprovedByBootstrap)
	}
	if g.IsPending("repo/existing") {
		t.Fatal("bootstrap-seeded task must not be pending")
	}

	// Window closes: a subsequently-appearing untrusted task is gated.
	if !g.FinishBootstrap() {
		t.Fatal("FinishBootstrap reported gate was not bootstrapping")
	}
	fresh := writeTaskDir(t, t.TempDir(), "repo/fresh", "y")
	armed, err = g.Admit(fresh)
	if err != nil {
		t.Fatalf("post-bootstrap Admit: %v", err)
	}
	if armed {
		t.Fatal("task appearing after bootstrap must be pending, got armed")
	}
	if _, ok := lock.Get("repo/fresh"); ok {
		t.Fatal("pending post-bootstrap task must not get a lock record")
	}
	if len(arm.armedIDs()) != 1 {
		t.Fatalf("armed = %v, want only the bootstrap-seeded task", arm.armedIDs())
	}
}

func TestTrustedSourceArmsAndRecords(t *testing.T) {
	p := enabledPolicy()
	p.TrustedSources["repo"] = true
	g, arm, lock := newTestGate(t, p)
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")

	armed, err := g.Admit(spec)
	if err != nil || !armed {
		t.Fatalf("Admit = (%v, %v), want (true, nil)", armed, err)
	}
	if got := arm.armedIDs(); len(got) != 1 || got[0] != "repo/deploy" {
		t.Fatalf("armed = %v", got)
	}
	rec, ok := lock.Get("repo/deploy")
	if !ok || rec.ApprovedBy != ApprovedByTrustedSource {
		t.Fatalf("lock record = %+v, ok=%v", rec, ok)
	}
	wantHash, err := ContentHash(spec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Hash != wantHash {
		t.Fatalf("lock hash = %q, want %q", rec.Hash, wantHash)
	}
	if err := g.FireGuard("repo/deploy"); err != nil {
		t.Fatalf("FireGuard on armed task: %v", err)
	}
}

// TestFireGuardVetoesEditedApprovedTask is the regression for #530: an approved
// local-source task whose files change on disk must not run its new code until
// re-approved, even before the reconciler re-hashes the source and re-pends the
// task. The runtime imports task files fresh per run, so FireGuard re-hashes the
// live dir rather than trusting the pending set alone.
func TestFireGuardVetoesEditedApprovedTask(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => 1")

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit = (%v, %v), want (false, nil) pending", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	approvedHash, _ := lock.Get("repo/deploy")
	if err := g.FireGuard("repo/deploy"); err != nil {
		t.Fatalf("FireGuard on freshly approved task: %v", err)
	}

	// Edit the task on disk. The reconciler has not yet re-admitted it, so the
	// pending set is empty and the lock still holds the old approved hash.
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"),
		[]byte("export default () => evil()"), 0o644); err != nil {
		t.Fatal(err)
	}
	if g.IsPending("repo/deploy") {
		t.Fatal("precondition: task must not yet be re-pended by the reconciler")
	}

	if err := g.FireGuard("repo/deploy"); !errors.Is(err, ErrPending) {
		t.Fatalf("FireGuard after edit = %v, want ErrPending (changed code must not run)", err)
	}
	if rec, _ := lock.Get("repo/deploy"); rec.Hash != approvedHash.Hash {
		t.Fatalf("lock hash mutated by an unapproved edit: %q -> %q", approvedHash.Hash, rec.Hash)
	}
}

// TestFireGuardAllowsEditedTrustedTask confirms the re-hash veto does not
// regress trust:always — a trusted source's edited task still fires, since its
// trust is not bound to a content hash.
func TestFireGuardAllowsEditedTrustedTask(t *testing.T) {
	p := enabledPolicy()
	p.TrustedSources["repo"] = true
	g, _, _ := newTestGate(t, p)
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")

	if armed, err := g.Admit(spec); err != nil || !armed {
		t.Fatalf("Admit = (%v, %v), want armed", armed, err)
	}
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.FireGuard("repo/deploy"); err != nil {
		t.Fatalf("edited trusted-source task must still fire: %v", err)
	}
}

// TestFireGuardFailsClosedForUnadmittedTask covers the startup window: a
// gate-enabled, non-trusted task that the gate has not yet admitted (registry
// registration races ahead of Admit) must be vetoed, not allowed through.
func TestFireGuardFailsClosedForUnadmittedTask(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	if err := g.FireGuard("repo/never-admitted"); !errors.Is(err, ErrPending) {
		t.Fatalf("FireGuard on un-admitted task = %v, want ErrPending (fail closed)", err)
	}

	// A disabled gate has no opinion — unknown tasks still fire.
	gOff, _, _ := newTestGate(t, Policy{Enabled: false, TrustedSources: map[string]bool{}, TrustedTasks: map[string]bool{}})
	if err := gOff.FireGuard("repo/never-admitted"); err != nil {
		t.Fatalf("disabled gate must not veto: %v", err)
	}
}

func TestTrustedTaskOverride(t *testing.T) {
	p := enabledPolicy()
	p.TrustedTasks["repo/deploy"] = true
	g, arm, lock := newTestGate(t, p)
	root := t.TempDir()

	armed, err := g.Admit(writeTaskDir(t, root, "repo/deploy", "x"))
	if err != nil || !armed {
		t.Fatalf("trusted task: Admit = (%v, %v)", armed, err)
	}
	if rec, _ := lock.Get("repo/deploy"); rec.ApprovedBy != ApprovedByTrustedTask {
		t.Fatalf("approved_by = %q, want %q", rec.ApprovedBy, ApprovedByTrustedTask)
	}

	// A sibling task in the same (untrusted) source still goes pending.
	armed, err = g.Admit(writeTaskDir(t, root, "repo/other", "y"))
	if err != nil {
		t.Fatalf("Admit sibling: %v", err)
	}
	if armed {
		t.Fatal("sibling task must be pending")
	}
	if got := arm.armedIDs(); len(got) != 1 {
		t.Fatalf("armed = %v", got)
	}
}

func TestBuiltinBypass(t *testing.T) {
	g, arm, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "x")

	armed, err := g.Admit(spec)
	if err != nil || !armed {
		t.Fatalf("Admit builtin = (%v, %v)", armed, err)
	}
	if got := arm.armedIDs(); len(got) != 1 || got[0] != "buildin/mcp" {
		t.Fatalf("armed = %v", got)
	}
	if rec, ok := lock.Get("buildin/mcp"); !ok || rec.ApprovedBy != ApprovedByBuiltin {
		t.Fatalf("lock record = %+v, ok=%v", rec, ok)
	}
}

func TestApproveArmsAndWritesLock(t *testing.T) {
	g, arm, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "v1")

	if armed, _ := g.Admit(spec); armed {
		t.Fatal("expected pending")
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got := arm.armedIDs(); len(got) != 1 || got[0] != "repo/deploy" {
		t.Fatalf("armed = %v", got)
	}
	rec, ok := lock.Get("repo/deploy")
	if !ok || rec.ApprovedBy != ApprovedByManual {
		t.Fatalf("lock record = %+v, ok=%v", rec, ok)
	}
	if g.IsPending("repo/deploy") {
		t.Fatal("approved task still pending")
	}

	// Re-admitting the unchanged task arms straight from the lock.
	armed, err := g.Admit(spec)
	if err != nil || !armed {
		t.Fatalf("re-admit approved = (%v, %v)", armed, err)
	}
	if rec2, _ := lock.Get("repo/deploy"); rec2 != rec {
		t.Fatalf("re-admit rewrote lock record: %+v → %+v", rec, rec2)
	}
}

func TestApproveNotPendingErrors(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	if err := g.Approve("repo/unknown"); err == nil {
		t.Fatal("expected error approving a non-pending task")
	}
}

func TestApproveArmFailureKeepsPending(t *testing.T) {
	g, arm, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "v1")
	if armed, _ := g.Admit(spec); armed {
		t.Fatal("expected pending")
	}
	arm.armErr = errors.New("engine rejected")
	if err := g.Approve("repo/deploy"); err == nil {
		t.Fatal("expected arm error to propagate")
	}
	if !g.IsPending("repo/deploy") {
		t.Fatal("failed approve must keep the task pending")
	}
}

// TestApproveRacingReAdmitPromotesCorrectSnapshot is the regression for the
// approve()/Admit race identified in the #642 follow-up security review:
// approve() commits the pending hash to dicode.lock (via g.lock.Record) —
// which is exactly what makes Admit's already-approved fast path reachable
// for that id/hash — and only afterward (past g.arm, which can be slow/do
// IO) promotes the matching pendingEntry into approvedFiles/approvedResolved.
//
// Without the fix, a concurrent re-Admit of the SAME task at the SAME hash
// landing in that window — a legitimate scenario: e.g. a source reload
// re-emitting EventAdded for a task whose content on disk didn't change —
// takes the already-approved fast path itself: it deletes the very
// pendingEntry approve() still needs (g.clearPending), and its
// snapshotApprovedIfMissing sees approvedFiles[id] already non-nil (the
// PRIOR approved generation's content) and skips refreshing it. approve()
// then resumes, finds its own pendingEntry gone, and silently gives up on
// promoting — so the newly-approved hash ends up paired with stale
// pre-promotion content in approvedFiles (and, were persistence enabled,
// in the on-disk cache too), corrupting the Diff baseline for good.
//
// Reproduced deterministically without real goroutines: the fake arm
// callback below — invoked by approve() strictly between its g.lock.Record
// call and its own pending->approvedFiles promotion — itself calls g.Admit
// with the exact same (now-approved) hash. That is precisely the
// interleaving the finding describes, driven from a single goroutine.
func TestApproveRacingReAdmitPromotesCorrectSnapshot(t *testing.T) {
	lock, err := LoadLock(filepath.Join(t.TempDir(), LockFileName))
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	g := NewGate(enabledPolicy(), lock, "", func(task.Kinded) error { return nil }, nil)

	root := t.TempDir()
	spec := writeTaskDir(t, root, "repo/deploy", "F0 content\n")

	// Get the task approved at F0 first, so approvedFiles[id] starts out
	// non-nil (holding the PRIOR approved generation) — the precondition
	// that makes snapshotApprovedIfMissing's "already cached, skip" guard
	// bite for the race below, instead of the (harmless) first-ever-Admit
	// case where approvedFiles[id] starts nil.
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit F0 = (%v, %v), want pending", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve F0: %v", err)
	}

	// Content changes to F1; the task re-pends at a new hash H1.
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("F1 content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit F1 = (%v, %v), want pending", armed, err)
	}

	// Install the race-injecting arm hook, then approve the F1 pend. The
	// injected Admit fires exactly once, from inside approve()'s own arm()
	// call — i.e. after g.lock.Record(id, H1, ...) has already landed but
	// before approve() gets back to its own pending->approvedFiles
	// promotion.
	var injected bool
	var reAdmitErr error
	g.arm = func(task.Kinded) error {
		if !injected {
			injected = true
			_, reAdmitErr = g.Admit(spec)
		}
		return nil
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve F1: %v", err)
	}
	if !injected {
		t.Fatal("arm hook never fired — test is not exercising the intended interleaving")
	}
	if reAdmitErr != nil {
		t.Fatalf("injected concurrent Admit(H1) during approve(): %v", reAdmitErr)
	}

	// The correct content (F1, the version actually just approved) must be
	// the promoted baseline — not F0, the stale pre-promotion snapshot the
	// race would otherwise leave behind.
	g.mu.Lock()
	files := g.approvedFiles["repo/deploy"]
	g.mu.Unlock()
	got, ok := files["task.js"]
	if !ok {
		t.Fatalf("approvedFiles missing task.js entirely: %+v", files)
	}
	if strings.Contains(got.Content, "F0 content") {
		t.Fatalf("approvedFiles still holds the stale pre-promotion F0 content: %q", got.Content)
	}
	if !strings.Contains(got.Content, "F1 content") {
		t.Fatalf("approvedFiles = %q, want it to contain the just-approved F1 content", got.Content)
	}
	if g.IsPending("repo/deploy") {
		t.Fatal("task still pending after a successful approve()")
	}
}

func TestHashChangeRePends(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "v1")

	if armed, _ := g.Admit(spec); armed {
		t.Fatal("expected pending")
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	approvedRec, _ := lock.Get("repo/deploy")

	// Content change → hash drifts from the approved record → pending again.
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("v2 — changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	armed, err := g.Admit(spec)
	if err != nil {
		t.Fatalf("Admit changed: %v", err)
	}
	if armed {
		t.Fatal("changed task must re-pend")
	}
	if err := g.FireGuard("repo/deploy"); !errors.Is(err, ErrPending) {
		t.Fatalf("FireGuard after change = %v, want ErrPending", err)
	}
	// The lock keeps the previously approved hash for drift inspection.
	if rec, ok := lock.Get("repo/deploy"); !ok || rec != approvedRec {
		t.Fatalf("lock record changed on re-pend: %+v → %+v (ok=%v)", approvedRec, rec, ok)
	}
}

func TestGateDisabledArmsAndKeepsInventory(t *testing.T) {
	p := Policy{Enabled: false, TrustedSources: map[string]bool{}, TrustedTasks: map[string]bool{}}
	g, arm, lock := newTestGate(t, p)
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")

	armed, err := g.Admit(spec)
	if err != nil || !armed {
		t.Fatalf("Admit with gate disabled = (%v, %v)", armed, err)
	}
	if got := arm.armedIDs(); len(got) != 1 {
		t.Fatalf("armed = %v", got)
	}
	rec, ok := lock.Get("repo/deploy")
	if !ok || rec.ApprovedBy != ApprovedByGateDisabled {
		t.Fatalf("inventory record = %+v, ok=%v", rec, ok)
	}
	if len(g.Pending()) != 0 {
		t.Fatalf("Pending() = %v with gate disabled", g.Pending())
	}
}

func TestForgetDropsLockAndPending(t *testing.T) {
	p := enabledPolicy()
	p.TrustedSources["repo"] = true
	g, _, lock := newTestGate(t, p)
	root := t.TempDir()

	if armed, _ := g.Admit(writeTaskDir(t, root, "repo/deploy", "x")); !armed {
		t.Fatal("trusted task should arm")
	}
	g.Forget("repo/deploy")
	if _, ok := lock.Get("repo/deploy"); ok {
		t.Fatal("Forget left lock entry behind")
	}

	// Pending task: Forget clears the pending set too.
	g2, _, _ := newTestGate(t, enabledPolicy())
	if armed, _ := g2.Admit(writeTaskDir(t, root, "repo/pending", "y")); armed {
		t.Fatal("expected pending")
	}
	g2.Forget("repo/pending")
	if g2.IsPending("repo/pending") {
		t.Fatal("Forget left task pending")
	}
}

func TestHashErrorHeldPending(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	g.SetHashFunc(func(task.Kinded) (string, error) { return "", errors.New("boom") })
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")

	armed, err := g.Admit(spec)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if armed {
		t.Fatal("unhashable untrusted task must be held pending")
	}
	if err := g.Approve("repo/deploy"); err == nil {
		t.Fatal("approving a task with no hash must fail")
	}
}

func TestSourceOf(t *testing.T) {
	cases := map[string]string{
		"buildin/mcp":           "buildin",
		"infra/backend/deploy":  "infra",
		"flat-task":             "",
		"/leading-slash":        "",
		"repo/":                 "repo",
		"buildin/nested/deeper": "buildin",
	}
	for id, want := range cases {
		if got := SourceOf(id); got != want {
			t.Errorf("SourceOf(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestContentHashFallsBackToSpecJSON(t *testing.T) {
	// Dir-less task (inline taskset entry): hash derives from the resolved spec.
	a := &task.Spec{ID: "inline/a"}
	b := &task.Spec{ID: "inline/a", Timeout: 99}
	ha, err := ContentHash(a)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	hb, err := ContentHash(b)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if ha == "" || ha == hb {
		t.Fatalf("fallback hashes not distinct: %q vs %q", ha, hb)
	}
}

func TestPendingHash(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "v1")

	if _, ok := g.PendingHash("repo/deploy"); ok {
		t.Fatal("PendingHash before Admit must report not-pending")
	}
	if armed, _ := g.Admit(spec); armed {
		t.Fatal("expected pending")
	}
	hash, ok := g.PendingHash("repo/deploy")
	if !ok || hash == "" {
		t.Fatalf("PendingHash = (%q, %v), want observed hash", hash, ok)
	}
	want, err := ContentHash(spec)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if hash != want {
		t.Fatalf("PendingHash = %q, want %q", hash, want)
	}
}

func TestApproveIfHashMatch(t *testing.T) {
	g, arm, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "v1")
	if armed, _ := g.Admit(spec); armed {
		t.Fatal("expected pending")
	}
	hash, _ := g.PendingHash("repo/deploy")

	if err := g.ApproveIfHash("repo/deploy", hash); err != nil {
		t.Fatalf("ApproveIfHash: %v", err)
	}
	if got := arm.armedIDs(); len(got) != 1 || got[0] != "repo/deploy" {
		t.Fatalf("armed = %v", got)
	}
	rec, ok := lock.Get("repo/deploy")
	if !ok || rec.ApprovedBy != ApprovedByToken || rec.Hash != hash {
		t.Fatalf("lock record = %+v, ok=%v", rec, ok)
	}
	if g.IsPending("repo/deploy") {
		t.Fatal("approved task still pending")
	}
}

func TestApproveIfHashMismatchRejected(t *testing.T) {
	g, arm, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "v1")
	if armed, _ := g.Admit(spec); armed {
		t.Fatal("expected pending")
	}
	staleHash, _ := g.PendingHash("repo/deploy")

	// Task content changes; the gate re-observes a new hash.
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("v2 — changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if armed, _ := g.Admit(spec); armed {
		t.Fatal("changed task must still be pending")
	}

	// The stale hash must not approve the new version.
	if err := g.ApproveIfHash("repo/deploy", staleHash); err == nil {
		t.Fatal("stale-hash approval must be rejected")
	}
	if !g.IsPending("repo/deploy") {
		t.Fatal("task must stay pending after rejected approval")
	}
	if got := arm.armedIDs(); len(got) != 0 {
		t.Fatalf("arm called despite rejection: %v", got)
	}
	if _, ok := lock.Get("repo/deploy"); ok {
		t.Fatal("lock must not be written on rejected approval")
	}

	// Empty hash never approves.
	if err := g.ApproveIfHash("repo/deploy", ""); err == nil {
		t.Fatal("empty-hash approval must be rejected")
	}
}

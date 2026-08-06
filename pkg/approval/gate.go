package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// BuiltinSource is the source namespace of tasks shipped with the binary.
// Tasks under it bypass the gate.
const BuiltinSource = "buildin"

// ErrPending is returned (wrapped) by FireGuard when a fire is vetoed because
// the task is awaiting approval.
var ErrPending = errors.New("task pending approval")

// Policy is the operator's trust declaration, derived from the approval
// block of dicode.yaml.
type Policy struct {
	// Enabled toggles the gate. When false everything arms, but the lock is
	// still maintained as a running inventory.
	Enabled bool
	// TrustedSources holds source names (first segment of a namespaced task
	// ID) with trust: always.
	TrustedSources map[string]bool
	// TrustedTasks holds full task IDs with trust: always.
	TrustedTasks map[string]bool
}

// Gate decides, for every task the reconciler registers, whether its
// triggers arm or the task is held pending operator approval. It maintains
// dicode.lock as the manifest of approved hashes and tracks the pending set.
//
// Decoupled from the trigger engine via the arm callback so it can be unit
// tested with a fake.
type Gate struct {
	policy Policy
	lock   *Lock
	arm    func(task.Kinded) error
	hashFn func(task.Kinded) (string, error)
	log    *zap.Logger

	mu          sync.Mutex
	pending     map[string]pendingEntry
	admitted    map[string]task.Kinded
	pendingHook func(k task.Kinded, hash string)
	bootstrap   bool

	// approvedFiles is the last-known-approved content snapshot per task ID
	// (dir-relative path -> file content), refreshed every time Admit treats
	// the current on-disk dir as approved content (the already-approved-hash
	// fast path and every auto-approve path, each guarded by
	// snapshotApprovedIfMissing so a repeat poll at an already-cached hash
	// skips the walk) and promoted from a pendingEntry's own files on a
	// successful approve(). The current pending (not yet approved) content
	// snapshot lives on pendingEntry.files itself (see below) rather than in
	// a standalone map.
	//
	// This map itself is in-memory only, like pending and admitted above, and
	// is rebuilt by re-admit within a process — but unlike those two, every
	// write to it is also mirrored to the on-disk snapshotCache (see
	// snapshot_cache.go's persistApprovedSnapshot), keyed by task ID and the
	// hash dicode.lock currently records as approved. On daemon restart,
	// Admit's first look at a task consults that cache
	// (loadCachedApprovedIfMissing) before falling through to the old
	// no-baseline behavior, so a task that is pending approval right after a
	// restart still gets a real diff baseline as long as it was ever approved
	// by a previous process — see docs/concepts/security.md's Pending-Change
	// Diff section. Dir-less (inline) tasks never get a snapshot (see
	// taskDirOf) — nothing to snapshot, so nothing to cache either.
	approvedFiles map[string]map[string]snapshotValue

	// approvedResolved is the rendered resolved-fields text of the
	// last-approved version, so Diff can show what the post-override
	// permissions, runtime and trigger actually were — including when the
	// change came from a taskset override outside the task directory, which
	// no directory snapshot can see. Persisted alongside approvedFiles, same
	// caveats as above.
	approvedResolved map[string]string

	// snapCache persists approvedFiles/approvedResolved to disk so a restart
	// doesn't lose the diff baseline (#642). nil disables persistence
	// entirely (every snapshotCache method tolerates a nil receiver) — set
	// this way when NewGate is given an empty cacheDir, e.g. by tests that
	// don't care about restart survival.
	snapCache *snapshotCache
}

// pendingEntry captures the task, the hash observed at decision time, and
// the content snapshot taken at that same moment (dir-relative path -> file
// content; nil for a dir-less task or a snapshot failure). hash and files
// are always written together in a single critical section (see Admit's
// default case) so a reader can never observe a hash paired with a snapshot
// from a different generation — the fix for a race where approve() could
// promote a snapshot that was stale relative to the pending hash it was
// being matched against (previously hash and files lived in two separate
// maps, updated under two separate lock acquisitions).
type pendingEntry struct {
	kinded   task.Kinded
	hash     string
	files    map[string]snapshotValue
	resolved string
}

// NewGate builds a Gate. arm is invoked for every task that passes the gate
// (and again from Approve); it must be safe to call from the reconciler
// goroutine and from whatever goroutine later calls Approve.
//
// cacheDir is the directory (normally a subdirectory of the daemon's
// data_dir) under which the approved-content snapshot cache is persisted —
// see snapshot_cache.go. An empty cacheDir disables persistence: Admit and
// approve behave exactly as before #642, with approvedFiles rebuilt purely
// in-memory and no diff baseline surviving a restart.
func NewGate(policy Policy, lock *Lock, cacheDir string, arm func(task.Kinded) error, log *zap.Logger) *Gate {
	if log == nil {
		log = zap.NewNop()
	}
	return &Gate{
		policy:           policy,
		lock:             lock,
		arm:              arm,
		hashFn:           ContentHash,
		log:              log,
		pending:          map[string]pendingEntry{},
		admitted:         map[string]task.Kinded{},
		approvedFiles:    map[string]map[string]snapshotValue{},
		approvedResolved: map[string]string{},
		snapCache:        newSnapshotCache(cacheDir),
	}
}

// SetHashFunc overrides the content-hash function (tests).
func (g *Gate) SetHashFunc(fn func(task.Kinded) (string, error)) { g.hashFn = fn }

// SetPendingHook installs the operator-notification hook. It is invoked only
// on the transition into pending — a task newly held, or a held task observed
// at a different content hash — never on a re-admit of an unchanged pending
// task, so a 30s reconcile loop cannot spam notifications. The hook is called
// without the gate lock held; it must not block (spawn a goroutine for any
// slow work).
func (g *Gate) SetPendingHook(fn func(k task.Kinded, hash string)) {
	g.mu.Lock()
	g.pendingHook = fn
	g.mu.Unlock()
}

// SetBootstrap toggles bootstrap mode. While on, Admit seeds (auto-approves +
// records) every task instead of holding it pending, so adopting the gate on
// an install with existing tasks does not strand them. The daemon enables it
// when dicode.lock is absent at startup and ends it once the initial source
// sync settles.
func (g *Gate) SetBootstrap(on bool) {
	g.mu.Lock()
	g.bootstrap = on
	g.mu.Unlock()
}

// FinishBootstrap ends bootstrap mode; tasks seen afterwards are gated
// normally. Idempotent; reports whether bootstrap was active.
func (g *Gate) FinishBootstrap() bool {
	g.mu.Lock()
	was := g.bootstrap
	g.bootstrap = false
	g.mu.Unlock()
	return was
}

// Bootstrapping reports whether the gate is in the first-run seeding window.
func (g *Gate) Bootstrapping() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.bootstrap
}

// Admit runs the gate for a newly registered (new or changed) task. It
// returns true when the task was armed, false when it is held pending.
// A non-nil error reports an arm/lock failure, not a pending decision.
func (g *Gate) Admit(k task.Kinded) (armed bool, err error) {
	id := k.TaskID()
	hash, hashErr := g.hashFn(k)
	if hashErr != nil {
		// Without a hash the task can be trusted (policy) but never
		// hash-approved: Approved("") is always false, and Record refuses
		// empty hashes, so the lock stays untouched.
		g.log.Warn("approval: content hash failed",
			zap.String("task", id), zap.Error(hashErr))
		hash = ""
	}

	// Retain the live task handle so FireGuard can re-hash the on-disk dir at
	// fire time — the runtime re-reads task files per run, so an edit that
	// lands between reconcile cycles must be caught when the task is fired.
	g.mu.Lock()
	g.admitted[id] = k
	g.mu.Unlock()

	// Restart recovery (#642): if this is the first time this process has
	// ever considered id, and a previous process's approval left a matching
	// snapshot on disk, adopt it as the in-memory baseline before making any
	// admit decision below — including the pending default case, which
	// otherwise never populates approvedFiles at all.
	g.loadCachedApprovedIfMissing(id)

	var by string
	switch {
	case SourceOf(id) == BuiltinSource:
		by = ApprovedByBuiltin
	case g.policy.TrustedTasks[id]:
		by = ApprovedByTrustedTask
	case g.policy.TrustedSources[SourceOf(id)]:
		by = ApprovedByTrustedSource
	case !g.policy.Enabled:
		by = ApprovedByGateDisabled
	case g.lock.Approved(id, hash):
		// Already approved at exactly this hash; keep the original record.
		g.clearPending(id)
		g.snapshotApprovedIfMissing(id, taskDirOf(k))
		g.recordApprovedResolved(id, k)
		g.persistApprovedSnapshot(id)
		return true, g.arm(k)
	case g.Bootstrapping():
		// Adoption window: seed the current inventory as approved rather
		// than strand pre-existing tasks behind a gate with no approve UI.
		by = ApprovedByBootstrap
	default:
		g.mu.Lock()
		prev, was := g.pending[id]
		g.mu.Unlock()

		// An unchanged pending hash means the on-disk content is guaranteed
		// byte-identical to what's already cached on prev.files — skip the
		// directory walk + reads entirely and reuse it. Only a genuinely new
		// hash (or the task's first time pending) re-snapshots.
		changed := !was || prev.hash != hash
		files := prev.files
		dir := taskDirOf(k)
		// A dir-backed task whose very first takeSnapshot call failed
		// (transient I/O error) is left with files == nil, and every later
		// Admit at that same (unchanged) hash sets changed = false — without
		// this second condition the walk below would never run again, and
		// Gate.Diff would report that task Incomplete forever even once the
		// transient failure has cleared.
		if changed || (dir != "" && files == nil) {
			// I/O stays outside the lock, same as before. Snapshot the
			// current (new, not-yet-approved) dir so a later Diff has the
			// exact content the operator is being asked to review.
			//
			// Two distinct "no new snapshot" cases must not be conflated:
			// a genuinely dir-less task (dir == "" — an inline taskset entry
			// with nothing to ever snapshot) correctly gets files == nil,
			// matching pendingEntry.files' nil-means-nothing-captured
			// convention. But a dir-backed task whose takeSnapshot call
			// merely failed (transient I/O error, already logged by
			// takeSnapshot) must NOT fall through to nil here: the task
			// really is pending on new, security-relevant content, and
			// discarding prev.files would make Gate.Diff silently report an
			// empty diff for it instead of showing (slightly stale) real
			// content. So: only overwrite files with the new snapshot when
			// the dir is non-empty AND the snapshot actually succeeded;
			// otherwise keep whatever files already held (nil for dir-less,
			// prev.files as a best-effort fallback for a snapshot failure).
			if dir == "" {
				files = nil
			} else if snap := g.takeSnapshot(id, dir, "pending"); snap != nil {
				files = snap
			}
		}

		g.mu.Lock()
		// hash and files are written together in one critical section: a
		// concurrent Approve/ApproveIfHash can never observe a pending[id]
		// whose hash and files disagree on which generation they describe.
		g.pending[id] = pendingEntry{kinded: k, hash: hash, files: files, resolved: resolvedFieldsText(k)}
		hook := g.pendingHook
		g.mu.Unlock()
		if hook != nil && changed {
			hook(k, hash)
		}
		return false, nil
	}

	// Auto-approve path (builtin / trusted / gate disabled / bootstrap): keep
	// the lock current as the running inventory, then arm. The current dir IS
	// the approved content on this path, so the baseline snapshot used by
	// Diff must track it — but only a genuine hash change means this
	// generation was never snapshotted. Checked (cheap: an in-memory map
	// lookup) before Record below overwrites the prior hash, since after that
	// Approved(id, hash) would trivially be true either way.
	hashUnchanged := g.lock.Approved(id, hash)
	g.clearPending(id)
	if hash != "" {
		if err := g.lock.Record(id, hash, by); err != nil {
			// Inventory write failure must not keep a trusted task from
			// running; surface it and arm anyway.
			g.log.Warn("approval: lock write failed",
				zap.String("task", id), zap.Error(err))
		}
	}
	if hashUnchanged {
		// Common case on every ~30s reconcile poll of an unchanged trusted
		// task: the cached baseline is already current, so skip the
		// directory walk entirely, same as before this fix.
		g.snapshotApprovedIfMissing(id, taskDirOf(k))
	} else {
		// The hash just changed (or this is the task's first Admit): the
		// cached baseline, if any, describes a stale generation. Refresh
		// unconditionally so a later Diff — reachable once trust is revoked
		// or the gate is re-enabled — compares against what was actually
		// last approved, not whatever was cached from an earlier version.
		g.snapshotApproved(id, taskDirOf(k))
	}
	g.recordApprovedResolved(id, k)
	g.persistApprovedSnapshot(id)
	return true, g.arm(k)
}

// taskDirOf returns k's on-disk task directory, mirroring the *task.Spec /
// *task.PipelineTask type switch in ContentHash — those are the only two
// task.Kinded implementations that carry a TaskDir. Returns "" for anything
// else (dir-less inline taskset entries), which the snapshot helpers below
// and Diff both treat as "nothing to snapshot / diff".
func taskDirOf(k task.Kinded) string {
	switch s := k.(type) {
	case *task.Spec:
		return s.TaskDir
	case *task.PipelineTask:
		return s.TaskDir
	default:
		return ""
	}
}

// takeSnapshot captures dir's current on-disk content for id's what-labeled
// snapshot (used in the warning log line only), without touching the gate
// lock. Returns nil for a dir-less task (nothing to snapshot) or a snapshot
// failure (logged, not fatal: Admit's arm/lock decision must not be blocked
// by a diff-support snapshot) — callers store the nil as-is, matching
// taskDirOf's dir-less contract and pendingEntry.files' nil-means-nothing-
// captured convention.
func (g *Gate) takeSnapshot(id, dir, what string) map[string]snapshotValue {
	if dir == "" {
		return nil
	}
	snap, err := snapshotDir(dir)
	if err != nil {
		g.log.Warn("approval: "+what+" snapshot failed", zap.String("task", id), zap.Error(err))
		return nil
	}
	return snap
}

// snapshotApproved unconditionally refreshes approvedFiles[id] from dir's
// current on-disk content. Called from every Admit path that treats the
// current dir as already-approved content. A dir-less task, or a snapshot
// failure, leaves approvedFiles[id] untouched (see takeSnapshot).
func (g *Gate) snapshotApproved(id, dir string) {
	snap := g.takeSnapshot(id, dir, "approved")
	if snap == nil {
		return
	}
	g.mu.Lock()
	g.approvedFiles[id] = snap
	g.mu.Unlock()
}

// snapshotApprovedIfMissing calls snapshotApproved only when approvedFiles[id]
// is not yet cached — after a daemon restart (nothing cached yet) or a
// task's first-ever Admit. A cache hit means the baseline is already current
// (kept current by approve()'s direct promotion on the gated path, or
// simply accepted as-is on the trust/bootstrap paths — see Gate's
// approvedFiles doc comment), so this skips the directory walk + reads on
// every repeat ~30s reconcile poll at an already-known task.
func (g *Gate) snapshotApprovedIfMissing(id, dir string) {
	g.mu.Lock()
	_, exists := g.approvedFiles[id]
	g.mu.Unlock()
	if exists {
		return
	}
	g.snapshotApproved(id, dir)
}

// recordApprovedResolved pins the resolved-fields text of content the gate has
// just treated as approved. Unlike the file snapshot this is refreshed
// unconditionally: it costs a marshal rather than a directory walk, and a
// stale value here would make Diff either miss a change or invent one.
func (g *Gate) recordApprovedResolved(id string, k task.Kinded) {
	d := resolvedFieldsText(k)
	g.mu.Lock()
	defer g.mu.Unlock()
	if d == "" {
		delete(g.approvedResolved, id)
		return
	}
	g.approvedResolved[id] = d
}

// Approve approves a pending task: records its observed hash in the lock and
// arms its triggers. Returns an error when the task is not pending.
func (g *Gate) Approve(id string) error {
	return g.approve(id, "", ApprovedByManual)
}

// ApproveIfHash approves a pending task only when the hash observed at
// gate-decision time equals hash. Token-link redemptions go through this so a
// token minted for one version of a task can never approve a later version:
// if the task content changed after the token was issued, the redemption is
// rejected and the task stays pending.
func (g *Gate) ApproveIfHash(id, hash string) error {
	if hash == "" {
		return fmt.Errorf("approve %q: empty hash", id)
	}
	return g.approve(id, hash, ApprovedByToken)
}

// approve is the shared approval path. A non-empty wantHash must match the
// pending entry's observed hash exactly.
func (g *Gate) approve(id, wantHash, by string) error {
	g.mu.Lock()
	ent, ok := g.pending[id]
	g.mu.Unlock()
	if !ok {
		return fmt.Errorf("task %q is not pending approval", id)
	}
	if ent.hash == "" {
		return fmt.Errorf("task %q has no computable content hash; cannot approve", id)
	}
	if wantHash != "" && ent.hash != wantHash {
		return fmt.Errorf("task %q changed since the approval was issued; re-review and approve the current version", id)
	}
	if err := g.lock.Record(id, ent.hash, by); err != nil {
		return fmt.Errorf("record approval for %q: %w", id, err)
	}
	if err := g.arm(ent.kinded); err != nil {
		return fmt.Errorf("arm %q: %w", id, err)
	}
	// Clear only the entry we approved: a concurrent Admit may have re-pended
	// the task at a newer hash, and that newer version must stay held. The
	// same guard gates promoting the pending snapshot -> approvedFiles: if a
	// newer pend raced in, cur.hash no longer equals ent.hash, so cur.files
	// (that newer, unapproved content) must not become the new baseline.
	// cur is read fresh here, so cur.files is guaranteed to be the exact
	// snapshot pendingEntry paired with cur.hash — see pendingEntry's doc
	// comment on why hash and files can never disagree on generation.
	g.mu.Lock()
	promoted := false
	if cur, ok := g.pending[id]; ok && cur.hash == ent.hash {
		delete(g.pending, id)
		if cur.files != nil {
			g.approvedFiles[id] = cur.files
			g.approvedResolved[id] = cur.resolved
			promoted = true
		}
	}
	g.mu.Unlock()
	// Persist outside the lock, mirroring every other persistApprovedSnapshot
	// call site (gate.go's Admit paths) — only when this call actually
	// promoted a snapshot, so a lost promotion race (see the comment above)
	// cannot persist a cache entry for a hash whose in-memory files were never
	// actually adopted here.
	if promoted {
		g.persistApprovedSnapshot(id)
	}
	return nil
}

// Forget handles task removal: drops the task from the pending set and from
// the lock. A re-added task goes through the gate from scratch.
func (g *Gate) Forget(id string) {
	g.clearPending(id)
	g.mu.Lock()
	delete(g.admitted, id)
	delete(g.approvedFiles, id)
	delete(g.approvedResolved, id)
	g.mu.Unlock()
	g.snapCache.delete(id)
	if err := g.lock.Remove(id); err != nil {
		g.log.Warn("approval: lock remove failed",
			zap.String("task", id), zap.Error(err))
	}
}

// Pending returns the sorted IDs of tasks held awaiting approval.
func (g *Gate) Pending() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.pending))
	for id := range g.pending {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// IsPending reports whether id is held awaiting approval.
func (g *Gate) IsPending(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.pending[id]
	return ok
}

// PendingHash returns the content hash observed when id was held pending,
// and whether id is pending at all. Approval tokens are bound to this hash.
func (g *Gate) PendingHash(id string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ent, ok := g.pending[id]
	if !ok {
		return "", false
	}
	return ent.hash, true
}

// FireGuard vetoes any fire of a task whose current on-disk content is not
// approved. Wired into the trigger engine so manual / chain / replay paths —
// which resolve tasks from the registry rather than from armed triggers —
// cannot run an unapproved task.
//
// Beyond the pending-set check it re-hashes the live task dir and rejects a
// task whose content no longer matches its approved hash. The runtime imports
// task files fresh on every run, so an edit that lands between reconcile
// cycles would otherwise execute new code under a stale approval before the
// gate re-pends it. Trusted / builtin tasks (and a disabled gate) skip the
// re-hash: their trust does not bind to a specific content hash.
func (g *Gate) FireGuard(taskID string) error {
	if g.IsPending(taskID) {
		return fmt.Errorf("%w: %s", ErrPending, taskID)
	}
	if g.trusted(taskID) {
		return nil
	}
	g.mu.Lock()
	k, ok := g.admitted[taskID]
	g.mu.Unlock()
	if !ok {
		// A gate-enabled, non-trusted task the gate has not yet admitted must
		// not run: fail closed rather than leave a startup window open between
		// registry registration and the gate's Admit.
		return fmt.Errorf("%w: %s (awaiting an approval decision)", ErrPending, taskID)
	}
	live, err := g.hashFn(k)
	if err != nil {
		return fmt.Errorf("%w: %s (content hash failed: %v)", ErrPending, taskID, err)
	}
	if !g.lock.Approved(taskID, live) {
		return fmt.Errorf("%w: %s (content changed since approval; re-approve to run)", ErrPending, taskID)
	}
	return nil
}

// trusted reports whether id is approved independent of its content hash:
// builtin tasks, operator-trusted tasks/sources, or a disabled gate. Mirrors
// the hash-independent auto-approve arms of Admit so FireGuard never vetoes a
// task Admit would have armed unconditionally.
func (g *Gate) trusted(id string) bool {
	switch {
	case SourceOf(id) == BuiltinSource:
		return true
	case g.policy.TrustedTasks[id]:
		return true
	case g.policy.TrustedSources[SourceOf(id)]:
		return true
	case !g.policy.Enabled:
		return true
	default:
		return false
	}
}

func (g *Gate) clearPending(id string) {
	g.mu.Lock()
	delete(g.pending, id)
	g.mu.Unlock()
}

// SourceOf returns the source namespace of a task ID — its first path
// segment (spec.entries key), e.g. "buildin" for "buildin/mcp". Returns ""
// for an un-namespaced ID, which matches no source trust entry.
func SourceOf(id string) string {
	if i := strings.IndexByte(id, '/'); i > 0 {
		return id[:i]
	}
	return ""
}

// contentHashDomain is the versioned domain-separation prefix for the
// dir-backed content hash. Bump the version whenever the folded field set or
// encoding changes so old lock entries can never collide with new ones.
const contentHashDomain = "dicode-approval-content-v1"

// redactedEnvValue replaces literal EnvEntry.Value contents in every hash
// input. dicode.lock is documented as committable and every other hash input
// is reconstructable from the repo, so folding a literal env value would put
// a low-entropy credential digest in the lock — an offline dictionary
// attack. Trade-off: value-content edits no longer re-pend; a value change
// does not widen capability (the entry's name/secret/from refs still fold),
// which matches the WebhookSecret exclusion rationale.
const redactedEnvValue = "<redacted>"

// sanitizePermissions returns p with every non-empty Env literal Value and
// Default replaced by redactedEnvValue. Default is included because
// envresolve injects it as the env var's value when the named secret is
// absent, so it holds the same class of material as Value and must not reach
// the hash in a low-entropy, offline-attackable form either. The Env slice is
// copied before mutation so the caller's spec is never touched;
// name/secret/from refs are kept.
func sanitizePermissions(p task.Permissions) task.Permissions {
	needsRedact := false
	for _, e := range p.Env {
		if e.Value != "" || e.Default != "" {
			needsRedact = true
			break
		}
	}
	if !needsRedact {
		return p
	}
	env := make([]task.EnvEntry, len(p.Env))
	copy(env, p.Env)
	for i := range env {
		if env[i].Value != "" {
			env[i].Value = redactedEnvValue
		}
		if env[i].Default != "" {
			env[i].Default = redactedEnvValue
		}
	}
	p.Env = env
	return p
}

// resolvedParam is the minimal override-mutable tuple of a task.Param folded
// into the hash. Description (and Type, which mergeParams cannot touch) are
// deliberately excluded so cosmetic param edits do not churn approvals.
// Default is carried raw here (ContentHash must stay sensitive to a
// repointed default — see resolvedSecurityFields.Params) and redacted only
// where resolvedFieldsText renders it for display — see there for why.
type resolvedParam struct {
	Name     string `json:"name"`
	Default  string `json:"default"`
	Required bool   `json:"required"`
}

// resolvedSecurityFields pins the exact set of resolved (post-override)
// security-bearing spec fields folded into the content hash. Keeping the set
// in a dedicated struct (rather than hashing the whole spec) makes it
// explicit and deterministic: cosmetic resolved fields (name, description,
// param descriptions, …) do not churn approvals, while anything that widens
// what the task may touch — or what it is fed — does.
//
// The reflection guard in content_hash_guard_test.go enforces that every
// field of task.Overrides / task.TriggerPatch is either represented here or
// explicitly exempted.
type resolvedSecurityFields struct {
	// Permissions is the full resolved permission set (env, fs, run, net,
	// sys, dicode) — taskset overrides can replace or merge any of these.
	// Env literal values are redacted (see redactedEnvValue).
	Permissions task.Permissions `json:"permissions"`
	// Runtime is folded in because overrides can swap deno→python, which
	// changes how (and whether) the declared permissions are enforced.
	Runtime task.Runtime `json:"runtime"`
	// Params are override-mutable (mergeParams) program inputs: an override
	// can repoint a param-default URL at an attacker endpoint without
	// touching the dir, so defaults must perturb the hash.
	Params []resolvedParam `json:"params,omitempty"`
	// Timeout is override-mutable and widens the task's wall-clock budget.
	Timeout time.Duration `json:"timeout"`
	// Trigger captures the full resolved trigger shape: an override's
	// TriggerPatch (pkg/task/overrides.go: Cron, Webhook, Auth, Manual,
	// Chain, Daemon, Restart) can switch a manual/cron task to an
	// (unauthenticated) webhook, change its path, or rewire chain/daemon —
	// none of which touch the dir hash. WebhookSecret and ReplayProtection
	// are deliberately excluded: TriggerPatch cannot set them, the secret
	// is already covered by the dir hash, and a secret must never feed a
	// non-secret hash input.
	Webhook     string               `json:"webhook"`
	WebhookAuth task.WebhookAuthMode `json:"webhook_auth"`
	Cron        string               `json:"cron"`
	Manual      bool                 `json:"manual"`
	Daemon      bool                 `json:"daemon"`
	Restart     string               `json:"restart"`
	Chain       *task.ChainTrigger   `json:"chain,omitempty"`
}

// resolvedPipelineSecurityFields mirrors resolvedSecurityFields for
// kind: PipelineTask. Pipelines skip taskset override layers in v1
// (pkg/taskset/resolver.go, case KindPipelineTask), but folding the resolved
// shape now means a future resolver that does apply overrides to pipelines
// fails closed (re-pends) instead of silently keeping a stale dir-only
// approval. PipelineTrigger has no Daemon/Restart; WebhookSecret and
// ReplayProtection are excluded for the same reasons as on Spec.
type resolvedPipelineSecurityFields struct {
	Timeout     time.Duration        `json:"timeout"`
	Webhook     string               `json:"webhook"`
	WebhookAuth task.WebhookAuthMode `json:"webhook_auth"`
	Cron        string               `json:"cron"`
	Manual      bool                 `json:"manual"`
	Chain       *task.ChainTrigger   `json:"chain,omitempty"`
}

// hashDirResolved combines the task-dir hash with the canonical JSON of the
// resolved security fields under the versioned domain prefix, NUL-delimited.
// hashInclude is forwarded to task.Hash unchanged — see task.Spec.HashInclude
// (#585) for why a task may need its content hash to cover files outside dir.
func hashDirResolved(taskID, dir string, resolved any, hashInclude ...string) (string, error) {
	dirHash, err := task.Hash(dir, hashInclude...)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(resolved)
	if err != nil {
		return "", fmt.Errorf("hash %s: marshal resolved fields: %w", taskID, err)
	}
	h := sha256.New()
	h.Write([]byte(contentHashDomain))
	h.Write([]byte{0})
	h.Write([]byte(dirHash))
	h.Write([]byte{0})
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ContentHash computes the gate's content hash for a task.
//
// For a *task.Spec with a task directory, the hash covers task.Hash over the
// directory (task.yaml + script files) AND a canonical JSON encoding of the
// resolved security-bearing fields (permissions, runtime, params, timeout,
// and the full resolved trigger shape). Folding the resolved fields in is
// essential: taskset overrides (pkg/taskset/override.go) mutate the resolved
// spec after load — they can replace permissions.net/fs/dicode, merge env
// entries, swap the runtime, repoint param defaults, widen the timeout, and
// patch the trigger (switch a manual/cron task to an unauthenticated
// webhook, change its path, rewire chain/daemon) — and taskset.yaml lives
// outside the task dir, so a dir-only hash would let an override elevate a
// task's effective permissions or exposure without ever re-pending it for
// approval (issue #400). The two inputs are combined under the versioned
// domain-separation prefix contentHashDomain, NUL-delimited.
//
// A *task.PipelineTask with a directory uses the same scheme over its
// resolved trigger shape and timeout — see resolvedPipelineSecurityFields.
//
// Dir-less tasks (inline taskset entries) hash the resolved spec JSON; for
// *task.Spec the marshalled copy has Trigger.WebhookSecret cleared and env
// literal values redacted (TriggerConfig carries yaml tags only, so every
// exported field — secret included — would otherwise marshal by Go name).
// resolvedPipelineFieldsOf builds the pipeline equivalent of
// resolvedSecurityFields.
func resolvedPipelineFieldsOf(s *task.PipelineTask) resolvedPipelineSecurityFields {
	return resolvedPipelineSecurityFields{
		Timeout:     s.Timeout,
		Webhook:     s.Trigger.Webhook,
		WebhookAuth: s.Trigger.WebhookAuth,
		Cron:        s.Trigger.Cron,
		Manual:      s.Trigger.Manual,
		Chain:       s.Trigger.Chain,
	}
}

// resolvedFieldsOf returns the post-override security fields ContentHash
// folds in alongside the directory digest, or nil for a kind that has none.
// Shared with ResolvedFieldsDigest so the two can never describe different
// field sets.
func resolvedFieldsOf(k task.Kinded) any {
	switch s := k.(type) {
	case *task.Spec:
		if s.TaskDir == "" {
			return nil
		}
		var params []resolvedParam
		for _, p := range s.Params {
			params = append(params, resolvedParam{
				Name:     p.Name,
				Default:  p.Default,
				Required: p.Required,
			})
		}
		return resolvedSecurityFields{
			Permissions: sanitizePermissions(s.Permissions),
			Runtime:     s.Runtime,
			Params:      params,
			Timeout:     s.Timeout,
			Webhook:     s.Trigger.Webhook,
			WebhookAuth: s.Trigger.WebhookAuth,
			Cron:        s.Trigger.Cron,
			Manual:      s.Trigger.Manual,
			Daemon:      s.Trigger.Daemon,
			Restart:     s.Trigger.Restart,
			Chain:       s.Trigger.Chain,
		}
	case *task.PipelineTask:
		if s.TaskDir == "" {
			return nil
		}
		return resolvedPipelineFieldsOf(s)
	default:
		return nil
	}
}

// resolvedFieldsText renders resolvedFieldsOf(k) as indented JSON for the
// diff to show directly, rather than merely detecting that it changed.
//
// These fields are what the content hash folds in beyond the directory's
// bytes, and taskset overrides rewrite them from outside the task directory.
// Comparing a digest could only ever say "something out here changed" — and
// could not tell that apart from an in-directory task.yaml edit, which alters
// the same resolved fields and is already visible in the file diff. Rendering
// the values instead means the operator sees the actual before/after whatever
// its origin, so no such distinction has to be drawn or explained.
//
// Env literal values are already redacted by sanitizePermissions, and
// WebhookSecret is excluded from resolvedSecurityFields entirely. Param
// defaults are redacted here, for display only — see redactParamsForDisplay
// for why they cannot be blanked in resolvedFieldsOf itself. Returns "" when
// there are no resolved fields.
func resolvedFieldsText(k task.Kinded) string {
	rf := resolvedFieldsOf(k)
	if rf == nil {
		return ""
	}
	b, err := json.MarshalIndent(redactParamsForDisplay(rf), "", "  ")
	if err != nil {
		return ""
	}
	return string(b) + "\n"
}

// redactParamsForDisplay returns rf with any resolvedSecurityFields.Params
// defaults blanked, for rendering only — resolvedFieldsOf's return value
// (unmodified) is what ContentHash hashes, and must stay that way.
//
// A param default is task-author-controlled ordinary program input — often a
// URL or a limit, not a secret — and TestContentHashFoldsParamDefault pins
// that an override repointing one (e.g. at an attacker endpoint) must still
// perturb the hash and re-pend the task; blanking it in resolvedFieldsOf
// would defeat that. But once rendered, this is the same surface
// redactValueLines already blanks task.Param.Default on for the file
// snapshot: "not secrets; that cost is accepted, since a param default is
// reconstructible from the task source while a leaked credential is not."
// A task author who wants a credential-shaped default is free to set one, so
// the same over-redaction tradeoff applies here — this has no field-path-
// aware way to tell a URL default from a credential one either.
func redactParamsForDisplay(rf any) any {
	sf, ok := rf.(resolvedSecurityFields)
	if !ok || len(sf.Params) == 0 {
		return rf
	}
	redacted := make([]resolvedParam, len(sf.Params))
	copy(redacted, sf.Params)
	for i := range redacted {
		if redacted[i].Default != "" {
			redacted[i].Default = redactedEnvValue
		}
	}
	sf.Params = redacted
	return sf
}

func ContentHash(k task.Kinded) (string, error) {
	switch s := k.(type) {
	case *task.Spec:
		if s.TaskDir != "" {
			return hashDirResolved(k.TaskID(), s.TaskDir, resolvedFieldsOf(k), s.HashInclude...)
		}
		// Dir-less fallback: hash a shallow copy with secrets stripped so the
		// committable lock never embeds a digest over secret material.
		c := *s
		c.Trigger.WebhookSecret = ""
		c.Permissions = sanitizePermissions(c.Permissions)
		return hashJSON(k.TaskID(), &c)
	case *task.PipelineTask:
		if s.TaskDir != "" {
			return hashDirResolved(k.TaskID(), s.TaskDir, resolvedPipelineFieldsOf(s))
		}
	}
	return hashJSON(k.TaskID(), k)
}

// hashJSON is the dir-less fallback: SHA-256 over the JSON encoding of v.
func hashJSON(taskID string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", taskID, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

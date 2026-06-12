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

	mu        sync.Mutex
	pending   map[string]pendingEntry
	bootstrap bool
}

// pendingEntry captures the task (and the hash observed at decision time) so
// Approve can record exactly what the operator saw and arm it.
type pendingEntry struct {
	kinded task.Kinded
	hash   string
}

// NewGate builds a Gate. arm is invoked for every task that passes the gate
// (and again from Approve); it must be safe to call from the reconciler
// goroutine and from whatever goroutine later calls Approve.
func NewGate(policy Policy, lock *Lock, arm func(task.Kinded) error, log *zap.Logger) *Gate {
	if log == nil {
		log = zap.NewNop()
	}
	return &Gate{
		policy:  policy,
		lock:    lock,
		arm:     arm,
		hashFn:  ContentHash,
		log:     log,
		pending: map[string]pendingEntry{},
	}
}

// SetHashFunc overrides the content-hash function (tests).
func (g *Gate) SetHashFunc(fn func(task.Kinded) (string, error)) { g.hashFn = fn }

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
		return true, g.arm(k)
	case g.Bootstrapping():
		// Adoption window: seed the current inventory as approved rather
		// than strand pre-existing tasks behind a gate with no approve UI.
		by = ApprovedByBootstrap
	default:
		g.mu.Lock()
		g.pending[id] = pendingEntry{kinded: k, hash: hash}
		g.mu.Unlock()
		return false, nil
	}

	// Auto-approve path (builtin / trusted / gate disabled): keep the lock
	// current as the running inventory, then arm.
	g.clearPending(id)
	if hash != "" {
		if err := g.lock.Record(id, hash, by); err != nil {
			// Inventory write failure must not keep a trusted task from
			// running; surface it and arm anyway.
			g.log.Warn("approval: lock write failed",
				zap.String("task", id), zap.Error(err))
		}
	}
	return true, g.arm(k)
}

// Approve approves a pending task: records its observed hash in the lock and
// arms its triggers. Returns an error when the task is not pending.
func (g *Gate) Approve(id string) error {
	g.mu.Lock()
	ent, ok := g.pending[id]
	g.mu.Unlock()
	if !ok {
		return fmt.Errorf("task %q is not pending approval", id)
	}
	if ent.hash == "" {
		return fmt.Errorf("task %q has no computable content hash; cannot approve", id)
	}
	if err := g.lock.Record(id, ent.hash, ApprovedByManual); err != nil {
		return fmt.Errorf("record approval for %q: %w", id, err)
	}
	if err := g.arm(ent.kinded); err != nil {
		return fmt.Errorf("arm %q: %w", id, err)
	}
	g.clearPending(id)
	return nil
}

// Forget handles task removal: drops the task from the pending set and from
// the lock. A re-added task goes through the gate from scratch.
func (g *Gate) Forget(id string) {
	g.clearPending(id)
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

// FireGuard vetoes any fire of a pending task. Wired into the trigger
// engine so manual / chain / replay paths — which resolve tasks from the
// registry rather than from armed triggers — cannot run an unapproved task.
func (g *Gate) FireGuard(taskID string) error {
	if g.IsPending(taskID) {
		return fmt.Errorf("%w: %s", ErrPending, taskID)
	}
	return nil
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

// ContentHash computes the gate's content hash for a task: task.Hash over
// the task directory (task.yaml + script files) when the task has one, else
// a hash of the resolved spec JSON (inline / dir-less tasks).
func ContentHash(k task.Kinded) (string, error) {
	var dir string
	switch s := k.(type) {
	case *task.Spec:
		dir = s.TaskDir
	case *task.PipelineTask:
		dir = s.TaskDir
	}
	if dir != "" {
		return task.Hash(dir)
	}
	b, err := json.Marshal(k)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", k.TaskID(), err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

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
		policy:   policy,
		lock:     lock,
		arm:      arm,
		hashFn:   ContentHash,
		log:      log,
		pending:  map[string]pendingEntry{},
		admitted: map[string]task.Kinded{},
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
		prev, was := g.pending[id]
		g.pending[id] = pendingEntry{kinded: k, hash: hash}
		hook := g.pendingHook
		g.mu.Unlock()
		if hook != nil && (!was || prev.hash != hash) {
			hook(k, hash)
		}
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
	// the task at a newer hash, and that newer version must stay held.
	g.mu.Lock()
	if cur, ok := g.pending[id]; ok && cur.hash == ent.hash {
		delete(g.pending, id)
	}
	g.mu.Unlock()
	return nil
}

// Forget handles task removal: drops the task from the pending set and from
// the lock. A re-added task goes through the gate from scratch.
func (g *Gate) Forget(id string) {
	g.clearPending(id)
	g.mu.Lock()
	delete(g.admitted, id)
	g.mu.Unlock()
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

// sanitizePermissions returns p with every non-empty Env literal Value
// replaced by redactedEnvValue. The Env slice is copied before mutation so
// the caller's spec is never touched; name/secret/from refs are kept.
func sanitizePermissions(p task.Permissions) task.Permissions {
	needsRedact := false
	for _, e := range p.Env {
		if e.Value != "" {
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
	}
	p.Env = env
	return p
}

// resolvedParam is the minimal override-mutable tuple of a task.Param folded
// into the hash. Description (and Type, which mergeParams cannot touch) are
// deliberately excluded so cosmetic param edits do not churn approvals.
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
func ContentHash(k task.Kinded) (string, error) {
	switch s := k.(type) {
	case *task.Spec:
		if s.TaskDir != "" {
			var params []resolvedParam
			for _, p := range s.Params {
				params = append(params, resolvedParam{
					Name:     p.Name,
					Default:  p.Default,
					Required: p.Required,
				})
			}
			return hashDirResolved(k.TaskID(), s.TaskDir, resolvedSecurityFields{
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
			}, s.HashInclude...)
		}
		// Dir-less fallback: hash a shallow copy with secrets stripped so the
		// committable lock never embeds a digest over secret material.
		c := *s
		c.Trigger.WebhookSecret = ""
		c.Permissions = sanitizePermissions(c.Permissions)
		return hashJSON(k.TaskID(), &c)
	case *task.PipelineTask:
		if s.TaskDir != "" {
			return hashDirResolved(k.TaskID(), s.TaskDir, resolvedPipelineSecurityFields{
				Timeout:     s.Timeout,
				Webhook:     s.Trigger.Webhook,
				WebhookAuth: s.Trigger.WebhookAuth,
				Cron:        s.Trigger.Cron,
				Manual:      s.Trigger.Manual,
				Chain:       s.Trigger.Chain,
			})
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

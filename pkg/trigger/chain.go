// This file contains chain-trigger dispatch: success-path trigger.chain
// edges, the config-level on_failure_chain with its storm / cooldown /
// concurrency guards, chain payload shaping, and success-chain cycle
// detection. The guard bookkeeping itself lives in engine_guardrails.go.

package trigger

import (
	"context"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
)

// maxSuccessChainDepth caps the _chain_depth for success-chain (trigger.chain)
// hops. Failure chains use OnFailureChainSpec.MaxDepth; success chains previously
// had no cap at all. A cap of 10 is generous for fan-out pipelines while still
// breaking accidental infinite loops that weren't caught at registration time.
const maxSuccessChainDepth = 10

// chainOnAlways is the trigger.chain `on:` value that fires the edge
// regardless of the upstream run's outcome. The other two accepted values
// compare directly against run statuses, so they use registry.StatusSuccess /
// registry.StatusFailure rather than their own constants.
const chainOnAlways = "always"

// hasSuccessChainCycle reports whether registering a task with ID newID that
// declares trigger.chain.from = from would close a success-chain cycle. It
// performs a DFS over the combined graph: existing registered tasks' success-chain
// edges plus the proposed new edge (from → newID). A cycle exists when newID
// can reach `from` via the combined graph — that would mean newID fires when
// `from` succeeds, and `from` would also eventually fire when newID succeeds,
// creating a loop.
//
// Caller must hold registerMu.
func (e *Engine) hasSuccessChainCycle(newID, from string) bool {
	// Build an adjacency list of existing success-chain edges: edge (A→B) means
	// B fires when A succeeds.
	successTargets := make(map[string][]string)
	for _, spec := range e.registry.All() {
		if spec.Trigger.Chain == nil {
			continue
		}
		on := spec.Trigger.Chain.ChainOn()
		if on != registry.StatusSuccess && on != chainOnAlways {
			continue
		}
		successTargets[spec.Trigger.Chain.From] = append(successTargets[spec.Trigger.Chain.From], spec.ID)
	}
	// Add the proposed new edge: from → newID.
	successTargets[from] = append(successTargets[from], newID)

	// DFS from newID: can we reach `from`? If yes, adding from→newID closes
	// a cycle because from→newID and newID→…→from form a loop.
	visited := make(map[string]bool)
	var dfs func(current string) bool
	dfs = func(current string) bool {
		if current == from {
			return true // reached the source → cycle
		}
		if visited[current] {
			return false
		}
		visited[current] = true
		for _, next := range successTargets[current] {
			if dfs(next) {
				return true
			}
		}
		return false
	}
	return dfs(newID)
}

// FireChain checks if any tasks declare a chain trigger from completedTaskID,
// and fires the global on_failure_chain if configured.
//
// upstreamParams is the upstream run's RunOptions.Params snapshot —
// piped through to the dispatch-time `${input.params.<name>}`
// resolver. Callers that don't track upstream params (e.g.
// preflight-short-circuit paths) pass nil.
func (e *Engine) FireChain(ctx context.Context, completedTaskID, runID, runStatus string, output interface{}, upstreamParams map[string]string) {
	// Pipeline stage re-fire hook: when a task that is a stage of a *live*
	// pipeline (terminal daemon stage running) re-fires successfully, replay
	// the descendant stages with fresh ${input} and restart the terminal
	// daemon so it picks up the freshly-rendered descendants. Failure or
	// cancel does NOT trigger a restart — see the failure-semantics tests
	// for the rationale.
	if runStatus == registry.StatusSuccess {
		e.handlePipelineStageRerun(completedTaskID, output)
	}
	// Shared resolver context for both the success-chain and
	// on_failure_chain dispatch paths below. The full upstream return
	// value flows through to the resolver — type-asserts happen
	// per-token (${input.output} requires string, ${input.output.X}
	// requires map).
	upstreamCtx := task.InputContext{Output: output, Params: upstreamParams}

	e.fireSuccessChains(ctx, completedTaskID, runID, runStatus, output, upstreamCtx)
	e.firePipelineChains(ctx, completedTaskID, runID, runStatus, output, upstreamCtx)
	e.fireFailureChain(ctx, completedTaskID, runID, runStatus, output, upstreamCtx)
}

// chainEdgeMatches reports whether a declared trigger.chain edge fires for
// the completion of completedTaskID with runStatus: the edge must point at
// the completed task (chain.From) and its ChainOn() value must be "always"
// or equal to the run status. Returns the resolved on-value for logging.
// Shared preamble of the kind: Task and kind: PipelineTask chain loops.
func chainEdgeMatches(chain *task.ChainTrigger, completedTaskID, runStatus string) (string, bool) {
	if chain == nil || chain.From != completedTaskID {
		return "", false
	}
	on := chain.ChainOn()
	if on != chainOnAlways && on != runStatus {
		return "", false
	}
	return on, true
}

// resolveChainEdge performs the dispatch-time `${input.…}` interpolation for
// one chain edge against the upstream context, logging and reporting
// ok=false on failure so the caller skips the edge rather than passing a
// literal token downstream. skipMsg distinguishes the kind: Task and kind:
// PipelineTask log lines, which predate this helper.
func (e *Engine) resolveChainEdge(params map[string]any, upstreamCtx task.InputContext, skipMsg, from, to string) (map[string]any, bool) {
	resolved, rerr := task.ResolveInputOutputMap(params, upstreamCtx)
	if rerr != nil {
		e.log.Error(skipMsg,
			zap.String("from", from),
			zap.String("to", to),
			zap.Error(rerr),
		)
		return nil, false
	}
	return resolved, true
}

// fireSuccessChains dispatches every kind: Task whose trigger.chain edge
// matches the completed run (declared chain triggers).
func (e *Engine) fireSuccessChains(ctx context.Context, completedTaskID, runID, runStatus string, output interface{}, upstreamCtx task.InputContext) {
	for _, spec := range e.registry.All() {
		chain := spec.Trigger.Chain
		on, matched := chainEdgeMatches(chain, completedTaskID, runStatus)
		if !matched {
			continue
		}
		// Per-edge overrides (#NNN): when the downstream's trigger.chain
		// declares `overrides:`, apply them to a deep copy of the
		// downstream spec before dispatching. The registry's canonical
		// spec is preserved so manual / cron / non-chain fires of the
		// same downstream see the on-disk values. The merged spec is
		// re-validated; on failure we log and skip this edge rather than
		// dispatching a malformed spec.
		dispatchSpec := spec
		if chain.Overrides != nil {
			merged := taskset.ApplyOverrides(spec, chain.Overrides)
			if vErr := merged.Validate(); vErr != nil {
				e.log.Error("chain trigger skipped — overrides produce invalid spec",
					zap.String("from", completedTaskID),
					zap.String("to", spec.ID),
					zap.Error(vErr),
				)
				continue
			}
			dispatchSpec = merged
		}
		// Dispatch-time `${input.…}` interpolation against the upstream
		// context (full return value + RunOptions.Params snapshot). The
		// resolver type-asserts per-token: ${input.output} requires a
		// string, ${input.output.X} requires a map, ${input.params.X}
		// requires a non-nil Params map with the named entry. Any
		// resolution failure skips the chain dispatch (logged) rather
		// than silently passing a literal token to the downstream.
		resolvedParams, resolved := e.resolveChainEdge(chain.Params, upstreamCtx,
			"chain trigger skipped — failed to resolve ${input.…} reference", completedTaskID, spec.ID)
		if !resolved {
			continue
		}

		// Depth tracking for success-chain (Fix 1, #387): mirror the failure-chain
		// depth cap so any cycle that slips past the registration-time DFS check
		// (e.g. tasks registered in a different order) cannot loop indefinitely.
		nextDepth := e.chainDepth(runID) + 1
		if nextDepth > maxSuccessChainDepth {
			e.log.Warn("chain trigger suppressed: max_depth exceeded",
				zap.Int("depth", nextDepth),
				zap.Int("max_depth", maxSuccessChainDepth),
				zap.String("from", completedTaskID),
				zap.String("to", spec.ID),
				zap.String("run", runID),
			)
			continue
		}

		e.log.Info("chain trigger",
			zap.String("from", completedTaskID),
			zap.String("to", spec.ID),
			zap.String("on", on),
			zap.Int("depth", nextDepth),
		)
		// Use buildChainPayload (not buildChainInput) to stamp _chain_depth so the
		// downstream can propagate it. When resolvedParams is empty, build a minimal
		// map with just engine keys so depth is always present.
		var chainInput interface{}
		if len(resolvedParams) == 0 && len(chain.Params) == 0 {
			// Historical no-params case: downstream expects raw output as input.
			// We must still propagate depth, so only add the wrapper when depth > 0
			// (depth 0 = first hop — keep raw output semantics for existing tasks).
			if nextDepth <= 1 {
				chainInput = output
			} else {
				chainInput = buildChainPayload(resolvedParams, completedTaskID, runID, runStatus, output, nextDepth)
			}
		} else {
			chainInput = buildChainPayload(resolvedParams, completedTaskID, runID, runStatus, output, nextDepth)
		}
		go e.fireAsync(ctx, dispatchSpec, pkgruntime.RunOptions{ //nolint:errcheck
			ParentRunID: runID,
			Input:       chainInput,
		}, "chain")
	}
}

// firePipelineChains dispatches every pipeline chain subscriber: a kind:
// PipelineTask may chain from an upstream task's outcome. chain.params (if
// set) are resolved against the upstream context and forwarded as the
// trigger payload for stage 0 (#350), mirroring the kind: Task chain
// dispatch path (fireSuccessChains).
func (e *Engine) firePipelineChains(ctx context.Context, completedTaskID, runID, runStatus string, output interface{}, upstreamCtx task.InputContext) {
	for _, k := range e.registry.AllKinded() {
		p, ok := k.(*task.PipelineTask)
		if !ok {
			continue
		}
		on, matched := chainEdgeMatches(p.Trigger.Chain, completedTaskID, runStatus)
		if !matched {
			continue
		}
		// Resolve any ${input.*} references in chain.params against the upstream
		// return value + params, exactly like the kind: Task chain path does.
		resolvedParams, resolved := e.resolveChainEdge(p.Trigger.Chain.Params, upstreamCtx,
			"pipeline chain trigger skipped — failed to resolve ${input.…} reference", completedTaskID, p.ID)
		if !resolved {
			continue
		}
		// Build the trigger payload for stage 0: mirror the kind: Task chain
		// path (buildChainInput) exactly:
		//   - Non-empty chain.params → wrap as a labelled map (taskID, runID,
		//     status, output, params) so stage 0 can access individual fields.
		//   - Empty/nil chain.params → thread the upstream's raw output directly
		//     (same as buildChainInput's zero-params branch) so ${input.output}
		//     on stage 0 resolves to the upstream return value.
		triggerInput := buildChainInput(resolvedParams, completedTaskID, runID, runStatus, output)
		triggerParams := flatStringMap(resolvedParams)
		e.log.Info("chain trigger (pipeline)",
			zap.String("from", completedTaskID), zap.String("to", p.ID), zap.String("on", on))
		go func(p *task.PipelineTask, in interface{}, params map[string]string) {
			if _, err := e.firePipeline(ctx, p, pkgruntime.RunOptions{
				ParentRunID: runID,
				Input:       in,
				Params:      params,
			}, registry.TriggerChain); err != nil {
				e.log.Warn("chain-triggered pipeline failed to start",
					zap.String("from", completedTaskID), zap.String("to", p.ID), zap.Error(err))
			}
		}(p, triggerInput, triggerParams)
	}
}

// fireFailureChain dispatches the config-level default on_failure_chain (or
// the failed task's per-task override) when the completed run failed,
// applying the replay-source, depth, storm, cooldown, and concurrency guards.
func (e *Engine) fireFailureChain(ctx context.Context, completedTaskID, runID, runStatus string, output interface{}, upstreamCtx task.InputContext) {
	// Config-level default on_failure_chain.
	if runStatus == registry.StatusFailure {
		chainSpec := e.defaultsOnFailureChain
		if failedSpec, ok := e.registry.Get(completedTaskID); ok {
			if failedSpec.OnFailureChain != nil {
				chainSpec = *failedSpec.OnFailureChain // per-task fully replaces defaults
			}
		}
		targetID := chainSpec.Task
		if targetID != "" && targetID != completedTaskID {
			// Replay-source guard: a replay-fired run that fails must not
			// trigger on_failure_chain — preserves the semantics that replay
			// is a manual / agent-initiated action with no auto-recovery loop.
			// (Spec § 4.3 #5.)
			if src, ok := e.runTriggerSource.Load(runID); ok {
				if ts, _ := src.(registry.TriggerSource); ts == registry.TriggerReplay {
					e.log.Debug("on_failure_chain skipped: replay source",
						zap.String("from", completedTaskID),
						zap.String("run", runID),
					)
					return
				}
			}
			// Depth-tracking ceiling: read the incoming run's chain depth and
			// refuse to fire when the next hop would exceed MaxDepth (default 2).
			// This replaces the old chain-of-chains suppression that blocked all
			// chaining beyond depth 1.
			nextDepth := e.chainDepth(runID) + 1
			maxDepth := chainSpec.EffectiveMaxDepth()
			if nextDepth > maxDepth {
				e.log.Warn("on_failure_chain suppressed: max_depth exceeded",
					zap.Int("depth", nextDepth),
					zap.Int("max_depth", maxDepth),
					zap.String("from", completedTaskID),
					zap.String("run", runID),
				)
				return
			}
			if targetSpec, ok := e.registry.Get(targetID); ok {
				now := time.Now()

				// 1. Storm check — per-source namespace.
				scope := stormScope(completedTaskID)
				if e.guards.stormSuppressed(scope, now) {
					e.log.Warn("on_failure_chain suppressed: storm circuit breaker tripped",
						zap.String("scope", scope),
						zap.String("from", completedTaskID),
					)
					return
				}

				// 2. Cooldown check.
				cd := effectiveCooldown(chainSpec)
				if e.guards.cooldownActive(completedTaskID, cd, now) {
					e.log.Warn("on_failure_chain suppressed: cooldown active",
						zap.String("from", completedTaskID),
						zap.Duration("cooldown", cd),
					)
					return
				}

				// 3. Concurrency check.
				perTask := chainSpec.MaxConcurrent
				if perTask <= 0 {
					perTask = 1
				}
				global := e.defaultsOnFailureChain.MaxConcurrentGlobal
				if global <= 0 {
					global = 3
				}
				if !e.guards.acquireSlot(completedTaskID, perTask, global) {
					e.log.Warn("on_failure_chain suppressed: concurrency cap reached",
						zap.String("from", completedTaskID),
						zap.Int("cap_per_task", perTask),
						zap.Int("cap_global", global),
					)
					return
				}

				// Dispatch-time `${input.…}` interpolation against the
				// upstream context. Symmetric with the success-chain
				// path above — any chain edge supports the recognised
				// shapes; failure-chain dispatch is skipped (logged) on
				// resolution failure rather than silently passing a
				// literal token to the downstream. Release the
				// chainGuards slot we just acquired so a failed
				// resolution doesn't burn cap_per_task / cap_global.
				resolvedParams, rerr := task.ResolveInputOutputMap(chainSpec.Params, upstreamCtx)
				if rerr != nil {
					e.guards.releaseSlot(completedTaskID)
					e.log.Error("on_failure_chain skipped — failed to resolve ${input.…} reference",
						zap.String("from", completedTaskID),
						zap.String("to", targetID),
						zap.Error(rerr),
					)
					return
				}
				e.log.Info("on_failure_chain trigger",
					zap.String("from", completedTaskID),
					zap.String("to", targetID),
					zap.String("run", runID),
					zap.Int("depth", nextDepth),
				)
				// Build input via the shared buildChainPayload kernel so the
				// failure-path and success-path stamps stay in lockstep.
				// Reserved keys (taskID, runID, status, output, _chain_depth)
				// are populated by the engine and are NOT user-overridable;
				// config-load validation (#236 Task 11) rejects any
				// chainSpec.Params containing these keys, so collisions
				// cannot reach here in a well-validated config.
				input := buildChainPayload(resolvedParams, completedTaskID, runID, runStatus, output, nextDepth)
				// Auto-fix safety default: when the chain target is the
				// buildin auto-fix preset, force mode=review unless the
				// operator explicitly set it. Autonomous-by-default would
				// be a foot-gun (the agent could push directly to main
				// without human review). Stamped AFTER the kernel so a
				// chainSpec.Params{mode:"yolo"} entry still wins.
				if targetID == "buildin/auto-fix" {
					if _, ok := input["mode"]; !ok {
						input["mode"] = "review"
					}
				}

				// Synchronously invoke fireAsync — it returns once startRun
				// completes, then continues the run on its own goroutine.
				// Doing this sync (rather than `go fireAsync(...)`) lets us
				// release the chainGuards slot if startRun fails and avoids
				// recording cooldown / storm counters for runs that never
				// executed (false-trip risk under DB flapping).
				if _, err := e.fireAsync(ctx, targetSpec, pkgruntime.RunOptions{
					ParentRunID:     runID,
					Input:           input,
					ChainParentTask: completedTaskID,
				}, registry.TriggerChain); err != nil {
					e.guards.releaseSlot(completedTaskID)
					e.log.Error("on_failure_chain fireAsync failed; released slot",
						zap.String("from", completedTaskID),
						zap.String("to", targetID),
						zap.Error(err),
					)
					return
				}

				// startRun succeeded. Record cooldown timestamp and storm
				// fire — both delayed until here so a failed startRun cannot
				// false-trip the storm breaker or extend the cooldown.
				e.guards.recordChainFire(completedTaskID, now)
				e.guards.observeChainFire(scope, chainSpec.Storm.Rate, chainSpec.Storm.Window,
					chainSpec.Storm.Suppress, now)
			}
		}
	}
}

// chainDepth returns the _chain_depth recorded for runID at fire time (see
// fireAsync), or 0 when none was stored — i.e. the run is a first hop.
func (e *Engine) chainDepth(runID string) int {
	if d, ok := e.runChainDepth.Load(runID); ok {
		depth, _ := d.(int)
		return depth
	}
	return 0
}

// buildChainInput shapes the `Input` value handed to a downstream task that
// was fired by a success-path trigger.chain (NOT on_failure_chain — that
// path runs through fireFailureChain and always wraps).
//
// Contract:
//
//   - When userParams is empty (the historical case), returns the upstream's
//     raw output unchanged. This preserves the existing contract for tasks
//     that consume `input` as the upstream's return value directly. Adding
//     a wrapping unconditionally would silently break every downstream task
//     in the wild that reads input as e.g. a string or a typed object.
//
//   - When userParams is non-empty, returns a map merging user params with
//     engine-reserved keys (taskID, runID, status, output, _chain_depth).
//     Reserved keys cannot collide with user params: Spec.validate rejects
//     reserved keys in trigger.chain.params at config load.
//
// _chain_depth is set to 0 for success chains. Depth tracking only matters
// on the failure path today (cap loops via OnFailureChainSpec.MaxDepth);
// success chains are not depth-capped because users build
// fan-out pipelines (render → start daemon, etc.) where depth > 1 is normal.
func buildChainInput(userParams map[string]any, completedTaskID, runID, status string, output any) any {
	if len(userParams) == 0 {
		return output
	}
	return buildChainPayload(userParams, completedTaskID, runID, status, output, 0)
}

// buildChainPayload is the shared kernel that produces the input map fed to
// any chained task — success-path (trigger.chain) AND failure-path
// (on_failure_chain). Both sites used to inline the same five engine-key
// stamps; the unification eliminates that drift (survey §5.4 / 6.2.3).
//
// Reserved keys (taskID, runID, status, output, _chain_depth) are always
// stamped *after* the userParams overlay, so a (well-validated) user map
// cannot collide. Config-load validation enforces the reserved-key
// invariant at three sites; this helper is the runtime backstop.
func buildChainPayload(userParams map[string]any, completedTaskID, runID, status string, output any, depth int) map[string]any {
	m := make(map[string]any, len(userParams)+5)
	for k, v := range userParams {
		m[k] = v
	}
	m["taskID"] = completedTaskID
	m["runID"] = runID
	m["status"] = status
	m["output"] = output
	m["_chain_depth"] = depth
	return m
}

// stormScope returns the namespace used for per-source storm tracking:
// everything before the last '/' in taskID, OR taskID itself for a flat
// (no-slash) task. Flat tasks get their own per-task scope so a single
// flapping flat task cannot suppress on_failure_chain for unrelated
// flat tasks under a shared "global" scope.
func stormScope(taskID string) string {
	if i := strings.LastIndex(taskID, "/"); i > 0 {
		return taskID[:i]
	}
	if taskID == "" {
		return "global"
	}
	return taskID
}

// effectiveCooldown returns the configured cooldown or 10 minutes when zero.
func effectiveCooldown(s task.OnFailureChainSpec) time.Duration {
	if s.Cooldown > 0 {
		return s.Cooldown
	}
	return 10 * time.Minute
}

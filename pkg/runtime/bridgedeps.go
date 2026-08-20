package runtime

import (
	"bufio"
	"context"
	"io"
	"path/filepath"
	"sync"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// BridgeDeps is the dependency-injection surface shared by the socket-bridge
// runtimes (Deno, Python). It carries everything a per-run IPC server needs
// (registry, secrets, capability handlers) plus the Set* wiring that
// daemon.go calls at boot. Both runtimes embed it, so the setters below are
// promoted onto *deno.Runtime and *python.Runtime; this replaces two
// hand-maintained copies of the same twelve setters (issue #388).
//
// Wiring happens in two phases:
//
//   - Construction-time fields (Registry, SecretsChain, DB, Log, IPCSecret)
//     are set by each runtime's New and snapshotted into per-version
//     executors by NewExecutor.
//   - Late-wired fields (InputStore, Replayer, SourceMgr, RepoResolver,
//     TestGuard, ProtectedPaths, SharedResolver) are set by daemon.go after
//     buildRuntimes returns. Per-version executors must therefore read them
//     live through their parent (manager) Runtime's BridgeDeps rather than
//     from their own snapshot — see each runtime's parent back-reference.
type BridgeDeps struct {
	Registry     *registry.Registry
	SecretsChain secrets.Chain
	// SecretsManager is optional; wired for dicode.secrets_set/delete.
	SecretsManager secrets.Manager
	// InputStore is optional; wired for dicode.runs.delete_input / get_input.
	InputStore *registry.InputStore
	// ResumeStateStore is optional; wired for dicode.runs.delete_resume_state
	// (#570) — the resume-state-cleanup buildin's GC path.
	ResumeStateStore *registry.ResumeStateStore
	DB               db.DB
	Log              *zap.Logger
	// IPCSecret is the per-daemon secret used to mint per-run IPC tokens.
	IPCSecret []byte
	Engine    ipc.EngineRunner
	Gateway   *ipc.Gateway
	// SecretOutputCh is opt-in: when set, every run wires it into the
	// per-run IPC server so a provider task's dicode.output(..., secret)
	// call is routed to the resolver awaiting it. Nil leaves the path inert.
	SecretOutputCh chan map[string]string
	// ProviderRunner is wired by the trigger engine at daemon startup so
	// the env resolver can spawn provider tasks for from: task:<id>
	// entries. Nil disables provider lookups; legacy paths still work.
	ProviderRunner envresolve.ProviderRunner
	// SharedResolver is the daemon-scoped env resolver whose TTL cache
	// survives across task launches (issue #242). When non-nil, runs use it
	// instead of constructing a fresh instance each time.
	SharedResolver *envresolve.Resolver
	// Replayer, SourceMgr, RepoResolver are wired after buildRuntimes
	// returns (same late-wiring pattern as InputStore).
	Replayer     *registry.Replayer      // optional; enables dicode.runs.replay
	SourceMgr    ipc.SourceDevModeSetter // optional; enables dicode.sources.set_dev_mode
	RepoResolver ipc.RepoPathResolver    // optional; enables dicode.git.commit_push
	// TestGuard is the approval gate's veto for dicode.tasks.test, forwarded
	// to every per-run IPC server. Nil means allow.
	TestGuard func(taskID string) error
	// MCPTokenMinter mints/revokes ephemeral per-run dicode MCP API keys
	// (SetMCPTokenMinter). Nil disables the ephemeral path: a task declaring
	// MCPTokenEnvName then gets whatever the secrets chain resolves for that
	// name, same as before this feature existed.
	MCPTokenMinter MCPTokenMinter
	// ProtectedPaths are files (dicode.lock, dicode.yaml) that hold approval
	// state and must never be writable by a task, even when a broad write
	// grant covers their directory. Deno emits them as --deny-write flags;
	// Python forwards them to the audit-hook guard policy's deny list.
	ProtectedPaths []string
}

// NewIPCServer constructs the per-run IPC socket server and applies the
// capability wiring common to both socket-bridge runtimes.
//
// Construction-time dependencies (IPCSecret, Registry, DB, Log, Engine,
// Gateway, SecretsManager, SecretOutputCh) are read from the receiver — the
// snapshot each runtime's NewExecutor made. Late-wired capabilities
// (InputStore, Replayer, SourceMgr, RepoResolver, TestGuard) are read from
// live, which per-version executors point at the manager's BridgeDeps so
// daemon wiring that happens after NewExecutor is still honored; the manager
// passes itself.
//
// Runtime-specific capabilities are wired by the caller afterwards (Deno
// adds SetCryptoHandler); all setters are inert until srv.Start.
func (d *BridgeDeps) NewIPCServer(runID string, spec *task.Spec, params map[string]string, input any, red *secrets.Redactor, live *BridgeDeps) *ipc.Server {
	srv := ipc.New(runID, spec.ID, d.IPCSecret, d.Registry, d.DB, params, input, d.Log, spec, d.Engine)
	srv.SetGateway(d.Gateway)
	srv.SetSecrets(d.SecretsManager)
	srv.SetInputStore(live.InputStore)
	srv.SetResumeStateStore(live.ResumeStateStore)
	srv.SetRedactor(red)
	srv.SetReplayer(live.Replayer)
	if m := live.SourceMgr; m != nil {
		srv.SetSourceManager(m)
	}
	if r := live.RepoResolver; r != nil {
		srv.SetRepoResolver(r)
	}
	if d.SecretOutputCh != nil {
		srv.SetSecretOutput(d.SecretOutputCh)
	}
	if g := live.TestGuard; g != nil {
		srv.SetTestGuard(g)
	}
	return srv
}

// StreamRunLog reads r line-by-line, redacts each line, and appends it to
// the run log at the given level; stream names the pipe ("stdout"/"stderr")
// for the scanner-error diagnostic. The scanner buffer is 64 KiB initial /
// 1 MiB max so a long single line can't kill the stream (issue #194).
// AppendLog failures are ignored (as both runtimes always did) and scanner
// errors are logged, not fatal.
//
// Blocks until r is exhausted — i.e. until the pipe closes on process exit.
// Callers run it in a goroutine after wg.Add(1); wg.Done is deferred here so
// a caller-side wg.Wait guarantees every line is flushed to the DB before
// the run returns (otherwise a caller fetching logs immediately after could
// see an empty list).
func (d *BridgeDeps) StreamRunLog(wg *sync.WaitGroup, r io.Reader, runID, stream, level string, red *secrets.Redactor) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		_ = d.Registry.AppendLog(context.Background(), runID, level, red.RedactString(scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		d.Log.Warn(stream+" scanner error", zap.String("run", runID), zap.Error(err))
	}
}

// SetEngine configures the engine runner used for dicode.run_task calls.
func (d *BridgeDeps) SetEngine(e ipc.EngineRunner) { d.Engine = e }

// SetGateway attaches the HTTP gateway so daemon tasks can call http.register.
func (d *BridgeDeps) SetGateway(g *ipc.Gateway) { d.Gateway = g }

// SetSecretsManager wires the secrets manager so tasks with permissions.dicode.secrets_write
// can call dicode.secrets_set() and dicode.secrets_delete().
func (d *BridgeDeps) SetSecretsManager(m secrets.Manager) { d.SecretsManager = m }

// SetInputStore wires the InputStore so the per-run IPC server can serve
// dicode.runs.delete_input and dicode.runs.get_input calls. Must be called
// before any run; mirrors the SetEngine / SetGateway pattern.
func (d *BridgeDeps) SetInputStore(is *registry.InputStore) { d.InputStore = is }

// SetResumeStateStore wires the ResumeStateStore so the per-run IPC server
// can serve dicode.runs.delete_resume_state calls (#570). Mirrors the
// SetInputStore wiring.
func (d *BridgeDeps) SetResumeStateStore(rs *registry.ResumeStateStore) { d.ResumeStateStore = rs }

// SetReplayer wires the Replayer so the per-run IPC server can serve
// dicode.runs.replay calls. Mirrors the SetInputStore wiring.
func (d *BridgeDeps) SetReplayer(r *registry.Replayer) { d.Replayer = r }

// SetSourceManager wires the source manager so the per-run IPC server can
// serve dicode.sources.set_dev_mode calls.
func (d *BridgeDeps) SetSourceManager(m ipc.SourceDevModeSetter) { d.SourceMgr = m }

// SetRepoResolver wires the repo-path resolver so the per-run IPC server
// can serve dicode.git.commit_push calls.
func (d *BridgeDeps) SetRepoResolver(r ipc.RepoPathResolver) { d.RepoResolver = r }

// SetTestGuard wires the approval gate's veto for dicode.tasks.test into
// every per-run IPC server. Nil means allow; mirrors the SetReplayer wiring.
func (d *BridgeDeps) SetTestGuard(g func(taskID string) error) { d.TestGuard = g }

// SetProtectedPaths records files that no task may ever write (dicode.lock,
// dicode.yaml — the approval-gate state); the deny always wins over any broad
// write grant a task declares. Empty entries are dropped and the rest are
// cleaned so "../" segments and redundant separators resolve before they
// reach the enforcement layer, keeping the deny set canonical.
func (d *BridgeDeps) SetProtectedPaths(paths []string) {
	cleaned := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		cleaned = append(cleaned, filepath.Clean(p))
	}
	d.ProtectedPaths = cleaned
}

// SetSecretOutputChannel wires the channel that receives provider tasks'
// secret maps. Called by the trigger engine before firing a task in
// "provider" mode.
func (d *BridgeDeps) SetSecretOutputChannel(ch chan map[string]string) {
	d.SecretOutputCh = ch
}

// SetProviderRunner wires the env-resolver's provider invocation. The
// trigger engine implements ProviderRunner and registers itself here at
// daemon startup. Nil disables provider task: lookups.
func (d *BridgeDeps) SetProviderRunner(p envresolve.ProviderRunner) {
	d.ProviderRunner = p
}

// SetEnvResolver wires the daemon-scoped env resolver whose TTL cache
// survives across task launches (issue #242). When set, runs use it instead
// of constructing a fresh instance each time.
func (d *BridgeDeps) SetEnvResolver(r *envresolve.Resolver) {
	d.SharedResolver = r
}

// SetMCPTokenMinter wires the ephemeral per-run MCP token minter: on a run
// whose spec declares permissions.env MCPTokenEnvName, ApplyMCPToken mints a
// token through it and revokes on every exit path. Nil (the default)
// disables the ephemeral path. Mirrors the SetReplayer / SetTestGuard
// late-wiring pattern — call after New and before Start.
func (d *BridgeDeps) SetMCPTokenMinter(m MCPTokenMinter) {
	d.MCPTokenMinter = m
}

// Package python executes Python task scripts via the managed uv binary.
//
// uv (https://github.com/astral-sh/uv) is a fast Python package manager and
// script runner that dicode downloads and caches automatically — no system
// Python or pip installation is required.
//
// # Execution model
//
// Each Run spawns a fresh uv subprocess connected to the same per-run Unix
// socket server used by the Deno runtime. An embedded Python shim (sdk.py)
// provides the same globals as the Deno SDK:
//
//	log, params, env, kv, input, output
//
// To return a value from a task, assign the module-level variable `result`:
//
//	result = {"count": 42}
//
// # PEP 723 inline dependencies
//
// uv supports inline dependency declarations inside the script:
//
//	# /// script
//	# dependencies = ["requests>=2.31", "boto3"]
//	# ///
//
// The runtime extracts any such block from task.py and places it at the top of
// the temporary wrapper file so that uv can parse it correctly.
package python

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	uvpkg "github.com/dicode/dicode/pkg/uv"
	"go.uber.org/zap"
)

//go:embed sdk/dicode_sdk.py
var sdkContent string

// Runtime is the ManagedRuntime implementation for Python+uv.
// It manages the uv binary lifecycle and creates socket-bridge Executors.
type Runtime struct {
	reg            *registry.Registry
	secrets        secrets.Chain
	secretsManager secrets.Manager      // optional; wired for dicode.secrets_set/delete
	inputStore     *registry.InputStore // optional; wired for dicode.runs.delete_input / get_input
	db             db.DB
	log            *zap.Logger
	secret         []byte
	engine         ipc.EngineRunner
	gateway        *ipc.Gateway
	// secretOutputCh is opt-in: when set, every Execute wires it into the
	// per-run IPC server so a provider task's dicode.output(..., secret=
	// True) call is routed to the resolver awaiting it. Nil leaves the
	// path inert (current behavior).
	secretOutputCh chan map[string]string
	// providerRunner is wired by the trigger engine at daemon startup so
	// the env resolver can spawn provider tasks for from: task:<id>
	// entries. Nil disables provider lookups; legacy paths still work.
	providerRunner envresolve.ProviderRunner
}

// SetEngine configures the engine runner used for dicode.run_task calls.
func (rt *Runtime) SetEngine(e ipc.EngineRunner) { rt.engine = e }

// SetGateway attaches the HTTP gateway so daemon tasks can call http.register.
func (rt *Runtime) SetGateway(g *ipc.Gateway) { rt.gateway = g }

// SetSecretsManager wires the secrets manager so tasks with permissions.dicode.secrets_write
// can call dicode.secrets_set() and dicode.secrets_delete().
func (rt *Runtime) SetSecretsManager(m secrets.Manager) { rt.secretsManager = m }

// SetInputStore wires the InputStore so the per-run IPC server can serve
// dicode.runs.delete_input and dicode.runs.get_input calls. Must be called
// before any Execute; mirrors the SetEngine / SetGateway pattern.
func (rt *Runtime) SetInputStore(is *registry.InputStore) { rt.inputStore = is }

// SetSecretOutputChannel wires the channel that receives provider tasks'
// secret maps. Called by the trigger engine before invoking Execute when
// the task is being launched in "provider" mode.
func (rt *Runtime) SetSecretOutputChannel(ch chan map[string]string) {
	rt.secretOutputCh = ch
}

// SetProviderRunner wires the env-resolver's provider invocation. The
// trigger engine implements ProviderRunner and registers itself here at
// daemon startup. Nil disables provider task: lookups.
func (rt *Runtime) SetProviderRunner(p envresolve.ProviderRunner) {
	rt.providerRunner = p
}

// New creates a Python Runtime manager.
func New(reg *registry.Registry, sc secrets.Chain, database db.DB, log *zap.Logger) (*Runtime, error) {
	secret, err := ipc.NewSecret()
	if err != nil {
		return nil, fmt.Errorf("python runtime: generate ipc secret: %w", err)
	}
	return &Runtime{reg: reg, secrets: sc, db: database, log: log, secret: secret}, nil
}

// --- ManagedRuntime interface ---

func (rt *Runtime) Name() string        { return "python" }
func (rt *Runtime) DisplayName() string { return "Python (uv)" }
func (rt *Runtime) Description() string {
	return "Python runtime managed by uv. Supports inline dependencies via PEP 723 (# /// script blocks). Full SDK globals: log, params, env, kv, input, output."
}
func (rt *Runtime) DefaultVersion() string { return uvpkg.DefaultVersion }

// BinaryPath returns the expected cache path for the uv binary at the given version.
func (rt *Runtime) BinaryPath(version string) (string, error) {
	return uvpkg.BinaryPath(version)
}

// IsInstalled reports whether the uv binary for the given version is cached.
func (rt *Runtime) IsInstalled(version string) bool {
	p, err := uvpkg.BinaryPath(version)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Install downloads and caches the uv binary for the given version.
func (rt *Runtime) Install(_ context.Context, version string) error {
	_, err := uvpkg.EnsureUv(version)
	return err
}

// NewExecutor returns an Executor that runs Python scripts via the uv binary
// at binaryPath, connected to the dicode socket-bridge SDK.
func (rt *Runtime) NewExecutor(binaryPath string) pkgruntime.Executor {
	return &executor{
		uvPath:         binaryPath,
		parent:         rt,
		reg:            rt.reg,
		secrets:        rt.secrets,
		secretsManager: rt.secretsManager,
		db:             rt.db,
		log:            rt.log,
		secret:         rt.secret,
		engine:         rt.engine,
		gateway:        rt.gateway,
		secretOutputCh: rt.secretOutputCh,
		providerRunner: rt.providerRunner,
	}
}

// --- executor ---

type executor struct {
	uvPath         string
	parent         *Runtime // back-reference for live lookups (inputStore, etc.)
	reg            *registry.Registry
	secrets        secrets.Chain
	secretsManager secrets.Manager
	// inputStore is not snapshotted here; read live from parent.inputStore
	// so late SetInputStore calls (daemon wires it after buildRuntimes) are
	// visible to all executors without any extra bookkeeping.
	db             db.DB
	log            *zap.Logger
	secret         []byte
	engine         ipc.EngineRunner
	gateway        *ipc.Gateway
	secretOutputCh chan map[string]string
	providerRunner envresolve.ProviderRunner
}

// Execute implements runtime.Executor.
func (e *executor) Execute(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	runID := opts.RunID
	result := &pkgruntime.RunResult{RunID: runID}
	status := registry.StatusSuccess

	defer func() {
		if ferr := e.reg.FinishRun(context.Background(), runID, status); ferr != nil {
			e.log.Error("finish run", zap.String("run", runID), zap.Error(ferr))
		}
	}()

	// Resolve declared env permissions. When the trigger engine ran
	// preflight (issue #235), it forwards the *Resolved here so we don't
	// re-spawn provider tasks. When opts.PreResolvedEnv is nil (legacy
	// callers, tests that bypass the engine), fall back to inline
	// resolution. Provider tasks (from: task:<id>) are spawned and batched
	// at most once per provider per launch; legacy paths (secret:,
	// env:NAME, bare) are preserved.
	var resolvedRes *envresolve.Resolved
	var err error
	if opts.PreResolvedEnv != nil {
		resolvedRes = opts.PreResolvedEnv
	} else {
		resolvedRes, err = envresolve.New(e.reg, e.secrets, e.providerRunner).Resolve(ctx, spec)
		if err != nil {
			status = registry.StatusFailure
			result.Error = err
			return result, nil
		}
	}
	resolved := resolvedRes.Env
	redactor := secrets.NewRedactor(resolvedRes.Secrets)

	// Read the user's task.py.
	scriptPath := spec.ScriptPath()
	if scriptPath == "" {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("script not found for task %s", spec.ID)
		return result, nil
	}
	scriptBytes, err := os.ReadFile(scriptPath) //nolint:gosec
	if err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("read script: %w", err)
		return result, nil
	}

	timeout := spec.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	mergedParams := mergeParams(spec.Params, opts.Params)

	srv := ipc.New(runID, spec.ID, e.secret, e.reg, e.db, mergedParams, opts.Input, e.log, spec, e.engine)
	srv.SetGateway(e.gateway)
	srv.SetSecrets(e.secretsManager)
	srv.SetInputStore(e.parent.inputStore)
	srv.SetRedactor(redactor)
	if e.secretOutputCh != nil {
		srv.SetSecretOutput(e.secretOutputCh)
	}
	socketPath, token, err := srv.Start(execCtx)
	if err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("start socket server: %w", err)
		return result, nil
	}
	defer srv.Stop()

	// Build the temporary wrapper file.
	wrapped, err := buildWrapper(scriptBytes)
	if err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("build wrapper: %w", err)
		return result, nil
	}

	tmpFile, err := os.CreateTemp("", "dicode-task-"+runID+"__*.py")
	if err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("create temp file: %w", err)
		return result, nil
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(wrapped); err != nil {
		tmpFile.Close()
		status = registry.StatusFailure
		result.Error = fmt.Errorf("write wrapper: %w", err)
		return result, nil
	}
	tmpFile.Close()

	cmd := exec.CommandContext(execCtx, e.uvPath, "run", tmpFile.Name()) //nolint:gosec
	cmd.Env = buildEnv(resolved, socketPath, token)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		status = registry.StatusFailure
		result.Error = err
		return result, nil
	}

	if err := cmd.Start(); err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("start uv: %w", err)
		return result, nil
	}

	// Stream uv/Python stderr to registry logs in real-time.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			_ = e.reg.AppendLog(context.Background(), runID, "warn", redactor.RedactString(scanner.Text()))
		}
	}()

	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	select {
	case retVal := <-srv.ReturnCh():
		result.ChainInput = retVal
		if out := srv.Output(); out != nil {
			result.ChainInput = out.Data
		}
		select {
		case <-doneCh:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}

	case exitErr := <-doneCh:
		select {
		case retVal := <-srv.ReturnCh():
			result.ChainInput = retVal
		default:
		}
		if out := srv.Output(); out != nil {
			result.ChainInput = out.Data
		}
		if exitErr != nil {
			if execCtx.Err() != nil {
				status = registry.StatusCancelled
			} else {
				status = registry.StatusFailure
			}
			result.Error = exitErr
		}
	}

	wg.Wait()
	return result, nil
}

// buildWrapper assembles the final Python file that uv will execute:
//
//  1. PEP 723 script block (extracted from the user script, if present) — must
//     be first so uv can parse inline dependencies.
//  2. The dicode SDK shim (sdk.py).
//  3. The user script body (script block stripped out).
//  4. Return-capture epilogue.
func buildWrapper(scriptBytes []byte) (string, error) {
	pep723, body := extractPEP723(string(scriptBytes))

	var w strings.Builder
	if pep723 != "" {
		w.WriteString(pep723)
		w.WriteString("\n")
	}
	w.WriteString("# === dicode SDK ===\n")
	w.WriteString(sdkContent)
	w.WriteString("\n# === task script ===\n")
	w.WriteString(body)
	w.WriteString("\n# === return capture ===\n")
	w.WriteString("import sys as _sys\n")
	w.WriteString("_asyncio_mod = _sys.modules['asyncio']\n")
	w.WriteString("_main = globals().get('main')\n")
	w.WriteString("if _main is not None and _asyncio_mod.iscoroutinefunction(_main):\n")
	w.WriteString("    result = _asyncio_mod.run(_main(log=log, kv=kv, params=params, env=env, input=input, output=output, mcp=mcp, dicode=dicode))\n")
	w.WriteString("_set_return(globals().get('result', None))\n")
	// Schedule close on _loop so it runs *after* any pending _fire coroutines
	// (the event loop is FIFO — tasks submitted before this will drain first).
	// Wrap in try/except so a timeout never marks a successful run as failed.
	w.WriteString("async def _dicode_close():\n")
	w.WriteString("    _writer.close()\n")
	w.WriteString("    await _writer.wait_closed()\n")
	w.WriteString("try:\n")
	w.WriteString("    _asyncio_mod.run_coroutine_threadsafe(_dicode_close(), _loop).result(timeout=5)\n")
	w.WriteString("except Exception:\n")
	w.WriteString("    pass\n")
	return w.String(), nil
}

// extractPEP723 splits a Python script into the PEP 723 script block (if any)
// and the remaining body. The script block is the first contiguous group of
// lines starting with "# /// script" and ending with "# ///".
func extractPEP723(src string) (block, body string) {
	lines := strings.Split(src, "\n")
	start := -1
	end := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if start == -1 && trimmed == "# /// script" {
			start = i
			continue
		}
		if start != -1 && end == -1 && trimmed == "# ///" {
			end = i
			break
		}
	}
	if start == -1 || end == -1 {
		return "", src
	}
	blockLines := lines[start : end+1]
	bodyLines := append(lines[:start:start], lines[end+1:]...)
	return strings.Join(blockLines, "\n"), strings.Join(bodyLines, "\n")
}

func mergeParams(specParams []task.Param, overrides map[string]string) map[string]string {
	out := make(map[string]string, len(specParams))
	for _, p := range specParams {
		if p.Default != "" {
			out[p.Name] = p.Default
		}
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

func buildEnv(resolved map[string]string, socketPath, token string) []string {
	env := append(os.Environ(), "DICODE_SOCKET="+socketPath, "DICODE_TOKEN="+token)
	for k, v := range resolved {
		env = append(env, k+"="+v)
	}
	return env
}

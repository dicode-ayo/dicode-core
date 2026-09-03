package trigger

// End-to-end coverage for the `${input.output}` interpolation
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
//     ACTUAL `template/` task loaded from disk — the same
//     spec/code that ships in dicode-core. That exercises the real
//     library task's `run_result.enabled: false` + in-memory cache
//     return path, and provides regression
//     coverage if buildin/template's contract drifts.
//
//  3. The non-string-upstream short-circuit is verified with a separate
//     on-disk Deno task returning a JSON object — buildin/template
//     returns only strings, so the negative case needs its own fixture.
//
// Every task in this file is a NORMAL on-disk dicode task fixture
// committed under tests/e2e/fixtures/tasks/input-output-* — the same
// pattern the Playwright e2e suite already uses for chain-target,
// hello-manual, etc. The Go test loads each fixture via task.LoadDir
// (production loader is the system under test for spec construction),
// then mutates spec.Trigger.Chain.Params on the downstream so a single
// fixture can drive both the bare-token and embedded-literal cases.
// No YAML/TS heredocs in this file — fixtures are editable, lintable,
// and runnable via `dicode tasks run` like any other task.
//
// `${input.output}` on pipeline stage overrides is covered by the
// pipeline e2e tests; that's out of scope here. We only cover the chain
// edge.
//
// Gated on real Deno (skipped via newTestEnv's t.Skipf) — same gate
// as TestE2E_PipelineTask_RealDeno.

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

// buildinTemplateDir returns the absolute path to the vendored copy of
// dicode-buildin's `template/` task (see testdata/UPSTREAM.md). Anchored via
// runtime.Caller so it works regardless of the test runner's CWD (go test
// sets CWD to the package dir, but worktree-relative anchors drift if the
// layout ever changes).
func buildinTemplateDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot anchor template path")
	}
	dir := filepath.Join(filepath.Dir(thisFile), "testdata", "template")
	if _, err := os.Stat(filepath.Join(dir, "task.yaml")); err != nil {
		t.Fatalf("template task.yaml not found at %s: %v", dir, err)
	}
	return dir
}

// inputOutputFixtureDir returns the absolute path to the on-disk
// pollFileContents waits up to timeout for path to exist + be readable,
// returning its content. Used to observe a task's on-disk side effect
// without racing the run-state assertion.
func pollFileContents(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			return string(b)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("file %s never appeared within %v", path, timeout)
	return ""
}

// fixture task directory under tests/e2e/fixtures/tasks/<name>/.
// Anchored the same way as buildinTemplateDir: via runtime.Caller so
// `go test` from any CWD finds the fixtures. This keeps the e2e
// fixtures co-located with the Playwright suite's fixtures
// (tests/e2e/fixtures/tasks/...) rather than buried in pkg/trigger/.
func inputOutputFixtureDir(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot anchor fixture path")
	}
	pkgDir := filepath.Dir(thisFile)               // .../pkg/trigger
	repoRoot := filepath.Dir(filepath.Dir(pkgDir)) // .../
	dir := filepath.Join(repoRoot, "tests", "e2e", "fixtures", "tasks", name)
	if _, err := os.Stat(filepath.Join(dir, "task.yaml")); err != nil {
		t.Fatalf("fixture task.yaml not found at %s: %v", dir, err)
	}
	return dir
}

// loadBuildinTemplateAs loads the real `template/` task
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

// loadInputOutputDownstream loads the shared input-output-downstream
// fixture and injects chain.params.value with the caller-supplied
// expression (typically "${input.output}" or an embedded literal).
//
// The fixture's on-disk task.yaml declares chain.from but no
// chain.params — the test parameterises that per case so a single
// fixture covers both the bare-token substitution and the
// embedded-literal pass-through scenarios. We re-validate after the
// mutation so any spec-level regression in the chain.params surface
// surfaces here rather than at dispatch time.
func loadInputOutputDownstream(t *testing.T, chainValue string) *task.Spec {
	t.Helper()
	spec, err := task.LoadDir(inputOutputFixtureDir(t, "input-output-downstream"))
	if err != nil {
		t.Fatalf("LoadDir input-output-downstream: %v", err)
	}
	if spec.Trigger.Chain == nil {
		t.Fatal("input-output-downstream fixture missing trigger.chain block")
	}
	spec.Trigger.Chain.Params = map[string]any{"value": chainValue}
	if err := spec.Validate(); err != nil {
		t.Fatalf("input-output-downstream re-validate after Params bind: %v", err)
	}
	return spec
}

// TestE2E_InputOutput_ChainParamsStringSubstitution drives the
// happy-path success-chain dispatch through real Deno: the REAL
// `template/` task loaded from disk renders a literal
// string ("hello from upstream" — no placeholders, so no env wiring
// needed) and feeds a downstream whose `chain.params.value` is the
// literal `${input.output}` token. The downstream's task body reads
// `input.value` and writes it to a marker file, proving the resolver
// substituted the token BEFORE the engine packaged the input for
// delivery.
//
// Using the real buildin/template task exercises the production code
// path (its `run_result.enabled: false` + in-memory cache return).
// If the engine accidentally routed chain
// delivery through the persisted `runs.return_value` column (which
// buildin/template suppresses), the marker file would never appear.
func TestE2E_InputOutput_ChainParamsStringSubstitution(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	markerDir := t.TempDir()
	markerPath := filepath.Join(markerDir, "greeting.marker")
	t.Setenv("MARKER_PATH", markerPath)

	e := newTestEnv(t)

	const upstreamID = "input-output-upstream"
	// No ${VAR} placeholders → buildin/template renders this verbatim
	// and the downstream sees it as input.value after token
	// substitution. Keeps the env-permission surface empty.
	const upstreamReturn = "hello from upstream"

	// Downstream — on-disk fixture loaded via task.LoadDir. We inject
	// the bare ${input.output} token as chain.params.value so the
	// resolver substitutes the upstream's return string before
	// dispatch. The downstream ID is the fixture dir basename
	// ("input-output-downstream"); its chain.from already points at
	// "input-output-upstream" so loadBuildinTemplateAs binds the
	// upstream to that exact ID.
	downstream := loadInputOutputDownstream(t, "${input.output}")
	downstreamID := downstream.ID
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
// Why this case uses a dedicated on-disk fixture rather than
// buildin/template: buildin/template's contract is "render a string
// from a template", so it can never return a non-string. The
// negative-case fixture (input-output-non-string-upstream) returns a
// JSON object explicitly.
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

	markerDir := t.TempDir()
	markerPath := filepath.Join(markerDir, "didrun.marker")
	t.Setenv("MARKER_PATH", markerPath)

	e := newTestEnv(t)

	// Downstream — input-output-noop-downstream fixture. Its on-disk
	// chain.from already points at "input-output-non-string-upstream"
	// so no rebind is needed; LoadDir sets spec.ID to the dir basename
	// "input-output-noop-downstream". We bind chain.params.value to
	// ${input.output} after load so the dispatcher has a token to
	// resolve — and so the resolver's short-circuit (because the
	// upstream returned a non-string) is what's being tested, not a
	// "no params, no resolution needed" pass-through. If the resolver
	// mistakenly fires the task body anyway, it drops a "ran" marker
	// we can detect.
	downstream, err := task.LoadDir(inputOutputFixtureDir(t, "input-output-noop-downstream"))
	if err != nil {
		t.Fatalf("LoadDir input-output-noop-downstream: %v", err)
	}
	if downstream.Trigger.Chain == nil {
		t.Fatal("input-output-noop-downstream fixture missing trigger.chain block")
	}
	downstream.Trigger.Chain.Params = map[string]any{"value": "${input.output}"}
	if err := downstream.Validate(); err != nil {
		t.Fatalf("noop downstream re-validate after Params bind: %v", err)
	}
	downstreamID := downstream.ID
	if err := e.reg.Register(downstream); err != nil {
		t.Fatalf("reg.Register downstream: %v", err)
	}

	// Upstream — input-output-non-string-upstream fixture returns a
	// JSON object. The resolver's per-token type-assert for
	// ${input.output} requires a string; a map yields
	// ErrInputUnavailable, which causes the chain dispatch to skip
	// with an error log. The downstream fixture's chain.from
	// references this ID directly so no rebind is needed.
	upstream, err := task.LoadDir(inputOutputFixtureDir(t, "input-output-non-string-upstream"))
	if err != nil {
		t.Fatalf("LoadDir input-output-non-string-upstream: %v", err)
	}
	upstreamID := upstream.ID
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

// TestE2E_InputOutput_EmbeddedTokenInterpolates pins the behaviour
// that any string containing one or more recognised `${input.…}` tokens
// is rewritten through ReplaceAllStringFunc, so an embedded reference
// like `"prefix-${input.output}-suffix"` reaches the downstream with
// the resolved value spliced in: `"prefix-<upstream return>-suffix"`.
//
// The hand-written tests in pkg/task/inputref_test.go cover this at
// the unit layer; this e2e pins it at the chain-dispatch boundary so a
// future regression to the narrow grammar can't slip through.
// Upstream is the REAL `template/` task — the rendered
// string is what flows into `input.output` via the in-memory cache.
func TestE2E_InputOutput_EmbeddedTokenInterpolates(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	markerDir := t.TempDir()
	markerPath := filepath.Join(markerDir, "embedded.marker")
	t.Setenv("MARKER_PATH", markerPath)

	e := newTestEnv(t)

	const upstreamID = "input-output-upstream"
	const upstreamReturn = "the-real-value"
	const chainValue = "prefix-${input.output}-suffix"
	const wantMarker = "prefix-the-real-value-suffix"

	// Downstream — same fixture as the happy-path test, with the
	// embedded literal bound to chain.params.value. The marker
	// receives the spliced result.
	downstream := loadInputOutputDownstream(t, chainValue)
	downstreamID := downstream.ID
	if err := e.reg.Register(downstream); err != nil {
		t.Fatalf("reg.Register downstream: %v", err)
	}

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

	got := waitForRunOfTask(t, e.engine, downstreamID, 30*time.Second)
	if got == nil {
		t.Fatal("downstream was not fired within the timeout")
	}
	if got.Status != registry.StatusSuccess {
		t.Errorf("downstream status = %q, want success", got.Status)
	}

	markerContent := pollFileContents(t, markerPath, 5*time.Second)
	if markerContent != wantMarker {
		t.Errorf("marker file = %q, want %q (embedded token was not interpolated)",
			markerContent, wantMarker)
	}
}

// TestE2E_InputOutput_ChainParamsOutputFieldSubstitution drives the
// post-#316 ${input.output.<field>} grammar end-to-end through real
// Deno. The upstream — the existing input-output-non-string-upstream
// fixture — returns the JSON object `{x: 1, y: "hello"}`. The
// downstream's chain.params.value references `${input.output.y}` so
// the resolver must lift the string field out of the upstream's
// structured return and splice it into the downstream's `input.value`.
// The marker file pins the substituted value end-to-end.
//
// This is the e2e companion to TestResolveInputOutputMap_OutputField_StringOK
// (unit-layer), closing out the new grammar's coverage at the
// chain-dispatch boundary.
func TestE2E_InputOutput_ChainParamsOutputFieldSubstitution(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	markerDir := t.TempDir()
	markerPath := filepath.Join(markerDir, "field.marker")
	t.Setenv("MARKER_PATH", markerPath)

	e := newTestEnv(t)

	// Downstream — input-output-downstream's task.ts reads
	// `input.value` and writes it to ${MARKER_PATH}. We rebind
	// chain.from to point at the non-string upstream (whose on-disk
	// chain.from would otherwise route to the buildin/template
	// upstream this test doesn't use) and inject the
	// `${input.output.y}` reference into chain.params.value.
	downstream, err := task.LoadDir(inputOutputFixtureDir(t, "input-output-downstream"))
	if err != nil {
		t.Fatalf("LoadDir input-output-downstream: %v", err)
	}
	if downstream.Trigger.Chain == nil {
		t.Fatal("input-output-downstream fixture missing trigger.chain block")
	}
	downstream.Trigger.Chain.From = "input-output-non-string-upstream"
	downstream.Trigger.Chain.Params = map[string]any{"value": "${input.output.y}"}
	if err := downstream.Validate(); err != nil {
		t.Fatalf("downstream re-validate after rebind: %v", err)
	}
	downstreamID := downstream.ID
	if err := e.reg.Register(downstream); err != nil {
		t.Fatalf("reg.Register downstream: %v", err)
	}

	// Upstream — returns `{x: 1, y: "hello"}` (per the fixture's
	// task.ts).
	upstream, err := task.LoadDir(inputOutputFixtureDir(t, "input-output-non-string-upstream"))
	if err != nil {
		t.Fatalf("LoadDir input-output-non-string-upstream: %v", err)
	}
	upstreamID := upstream.ID
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

	got := waitForRunOfTask(t, e.engine, downstreamID, 30*time.Second)
	if got == nil {
		t.Fatal("downstream was not fired within the timeout")
	}
	if got.Status != registry.StatusSuccess {
		t.Errorf("downstream status = %q, want success", got.Status)
	}

	markerContent := pollFileContents(t, markerPath, 5*time.Second)
	if markerContent != "hello" {
		t.Errorf("marker file = %q, want %q (output.field did not resolve)", markerContent, "hello")
	}
}

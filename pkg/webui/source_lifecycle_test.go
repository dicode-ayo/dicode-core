package webui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
)

// fakeSourceConfig is an in-memory sourceConfigStore. persistErr is returned
// after a successful mutate, the way a failed dicode.yaml write is.
type fakeSourceConfig struct {
	mu         sync.RWMutex
	cfg        *config.Config
	persistErr error
	persists   int
}

func newFakeSourceConfig() *fakeSourceConfig {
	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{}
	return &fakeSourceConfig{cfg: cfg}
}

func (f *fakeSourceConfig) update(mutate func(*config.Config) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := mutate(f.cfg); err != nil {
		return err
	}
	f.persists++
	return f.persistErr
}

func (f *fakeSourceConfig) view(read func(*config.Config)) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	read(f.cfg)
}

func (f *fakeSourceConfig) entry(name string) *taskset.Entry {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.cfg.Spec.Entries[name]
}

// fakeSourceReconciler mirrors registry.Reconciler's bookkeeping: a source
// counts as running only once AddSource returns, so a RemoveSource for an id
// still inside AddSource misses. AddSource for an id with a gate blocks until
// the gate is closed; failAdd makes AddSource for that id fail.
type fakeSourceReconciler struct {
	mu      sync.Mutex
	running map[string]source.Source
	gates   map[string]chan struct{}
	failAdd map[string]error
	entered chan string
	adds    int
}

func newFakeSourceReconciler() *fakeSourceReconciler {
	return &fakeSourceReconciler{
		running: map[string]source.Source{},
		gates:   map[string]chan struct{}{},
		failAdd: map[string]error{},
		entered: make(chan string, 16),
	}
}

// block makes the next AddSource(id) wait until the returned func is called.
func (f *fakeSourceReconciler) block(id string) (release func()) {
	gate := make(chan struct{})
	f.mu.Lock()
	f.gates[id] = gate
	f.mu.Unlock()
	return func() { close(gate) }
}

func (f *fakeSourceReconciler) AddSource(src source.Source) error {
	id := src.ID()
	f.mu.Lock()
	f.adds++
	gate := f.gates[id]
	delete(f.gates, id)
	f.mu.Unlock()
	f.entered <- id
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failAdd[id]; err != nil {
		return err
	}
	f.running[id] = src
	return nil
}

func (f *fakeSourceReconciler) RemoveSource(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, id)
}

func (f *fakeSourceReconciler) runningIDs() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for id := range f.running {
		out[id] = true
	}
	return out
}

type lifecycleHarness struct {
	cfg *fakeSourceConfig
	rec *fakeSourceReconciler
	mgr *SourceManager
	l   *SourceLifecycle
}

func newLifecycleHarness(t *testing.T) *lifecycleHarness {
	t.Helper()
	h := &lifecycleHarness{cfg: newFakeSourceConfig(), rec: newFakeSourceReconciler()}
	h.mgr = NewSourceManager(h.cfg.cfg, nil, nil, t.TempDir(), zap.NewNop())
	h.l = newSourceLifecycle(h.cfg, h.rec, h.mgr, t.TempDir(), zap.NewNop())
	return h
}

func localRef(path string) taskset.Ref {
	watch := true
	return taskset.Ref{Path: path, Watch: &watch}
}

// assertConsistent checks the lifecycle invariant at quiescence: every config
// entry has exactly its own source registered and running, and nothing is
// registered or running without a matching entry.
func (h *lifecycleHarness) assertConsistent(t *testing.T) {
	t.Helper()
	h.cfg.mu.RLock()
	want := map[string]string{}
	for name, e := range h.cfg.cfg.Spec.Entries {
		want[name] = taskset.SourceID(name, e.Ref)
	}
	h.cfg.mu.RUnlock()

	h.mgr.mu.RLock()
	registered := map[string]string{}
	for name, src := range h.mgr.tasksets {
		registered[name] = src.ID()
	}
	h.mgr.mu.RUnlock()

	running := h.rec.runningIDs()

	for name, id := range registered {
		if want[name] != id {
			t.Errorf("registry has %q → %q, config entry wants %q", name, id, want[name])
		}
	}
	for name, id := range want {
		if registered[name] != id {
			t.Errorf("config entry %q (%s) is not registered (registry has %q)", name, id, registered[name])
		}
		if !running[id] {
			t.Errorf("config entry %q (%s) is not running in the reconciler", name, id)
		}
	}
	wantIDs := map[string]bool{}
	for _, id := range want {
		wantIDs[id] = true
	}
	for id := range running {
		if !wantIDs[id] {
			t.Errorf("reconciler still runs %q with no matching config entry", id)
		}
	}
}

func waitEntered(t *testing.T, rec *fakeSourceReconciler, id string) {
	t.Helper()
	select {
	case got := <-rec.entered:
		if got != id {
			t.Fatalf("AddSource entered for %q, want %q", got, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("AddSource(%q) never entered", id)
	}
}

func waitErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Add never returned")
		return nil
	}
}

// A Remove landing while Add's AddSource is in flight misses in the
// reconciler; Add must notice its claim is gone and stop its own source
// instead of registering an orphan.
func TestSourceLifecycle_ConcurrentRemove_NoOrphan(t *testing.T) {
	h := newLifecycleHarness(t)
	const name = "race-add-remove"
	ref := localRef("/tmp/" + name)
	id := taskset.SourceID(name, &ref)

	release := h.rec.block(id)
	addDone := make(chan error, 1)
	go func() { addDone <- h.l.Add(name, ref) }()
	waitEntered(t, h.rec, id)

	if err := h.l.Remove(name); err != nil {
		t.Fatalf("Remove during in-flight Add: %v", err)
	}
	release()
	if err := waitErr(t, addDone); !errors.Is(err, ErrSourceRemovedConcurrently) {
		t.Fatalf("Add error = %v, want ErrSourceRemovedConcurrently", err)
	}

	if e := h.cfg.entry(name); e != nil {
		t.Fatalf("config entry %q came back after Remove", name)
	}
	if _, ok := h.mgr.Get(name); ok {
		t.Fatalf("source %q registered with no config entry", name)
	}
	h.assertConsistent(t)
}

// Remove then re-Add of the same name and ref while the first Add is still
// in AddSource: both derive the same Source.ID, so the re-Add must be refused
// rather than let the first Add's teardown stop the newer source.
func TestSourceLifecycle_ReAddSameRefDuringSlowAdd_Busy(t *testing.T) {
	h := newLifecycleHarness(t)
	const name = "race-readd"
	ref := localRef("/tmp/race-readd")
	id := taskset.SourceID(name, &ref)

	release := h.rec.block(id)
	addA := make(chan error, 1)
	go func() { addA <- h.l.Add(name, ref) }()
	waitEntered(t, h.rec, id)

	if err := h.l.Remove(name); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	addB := make(chan error, 1)
	go func() { addB <- h.l.Add(name, ref) }()
	if err := waitErr(t, addB); !errors.Is(err, ErrSourceBusy) {
		t.Fatalf("re-Add during in-flight Add: error = %v, want ErrSourceBusy", err)
	}
	if e := h.cfg.entry(name); e != nil {
		t.Fatalf("refused re-Add left a config entry: %+v", e)
	}

	release()
	if err := waitErr(t, addA); !errors.Is(err, ErrSourceRemovedConcurrently) {
		t.Fatalf("Add A error = %v, want ErrSourceRemovedConcurrently", err)
	}
	h.assertConsistent(t)

	if err := h.l.Add(name, ref); err != nil {
		t.Fatalf("re-Add after the first Add finished: %v", err)
	}
	h.assertConsistent(t)
}

// A different ref derives a different Source.ID, but the name is still
// refused while its first Add is in flight.
func TestSourceLifecycle_ReAddOtherRefDuringSlowAdd_Busy(t *testing.T) {
	h := newLifecycleHarness(t)
	const name = "race-aba"
	refA := localRef("/tmp/race-aba-a")
	refC := localRef("/tmp/race-aba-c")
	idA := taskset.SourceID(name, &refA)

	releaseA := h.rec.block(idA)
	addA := make(chan error, 1)
	go func() { addA <- h.l.Add(name, refA) }()
	waitEntered(t, h.rec, idA)

	if err := h.l.Remove(name); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := h.l.Add(name, refC); !errors.Is(err, ErrSourceBusy) {
		t.Fatalf("Add C error = %v, want ErrSourceBusy", err)
	}

	releaseA()
	if err := waitErr(t, addA); !errors.Is(err, ErrSourceRemovedConcurrently) {
		t.Fatalf("Add A error = %v, want ErrSourceRemovedConcurrently", err)
	}
	if running := h.rec.runningIDs(); running[idA] {
		t.Fatalf("A's source %q left running after losing its claim", idA)
	}
	h.assertConsistent(t)
}

func TestSourceLifecycle_AddFailure_RollsBackClaim(t *testing.T) {
	h := newLifecycleHarness(t)
	const name = "fails"
	ref := localRef("/tmp/fails")
	startErr := errors.New("boom")
	h.rec.failAdd[taskset.SourceID(name, &ref)] = startErr

	err := h.l.Add(name, ref)
	if !errors.Is(err, ErrSourceStart) || !errors.Is(err, startErr) {
		t.Fatalf("Add error = %v, want ErrSourceStart wrapping %v", err, startErr)
	}
	if e := h.cfg.entry(name); e != nil {
		t.Fatalf("claim for %q not rolled back", name)
	}
	if _, ok := h.mgr.Get(name); ok {
		t.Fatalf("failed source %q registered", name)
	}
	h.assertConsistent(t)
}

// A failed Add clears its in-flight mark, so the name can be added again.
func TestSourceLifecycle_AddFailure_ClearsInFlight(t *testing.T) {
	h := newLifecycleHarness(t)
	const name = "fails-late"
	refA := localRef("/tmp/fails-late-a")
	idA := taskset.SourceID(name, &refA)
	h.rec.failAdd[idA] = errors.New("boom")

	releaseA := h.rec.block(idA)
	addA := make(chan error, 1)
	go func() { addA <- h.l.Add(name, refA) }()
	waitEntered(t, h.rec, idA)

	if err := h.l.Add(name, localRef("/tmp/fails-late-b")); !errors.Is(err, ErrSourceBusy) {
		t.Fatalf("Add B while A is in flight: error = %v, want ErrSourceBusy", err)
	}
	releaseA()
	if err := waitErr(t, addA); !errors.Is(err, ErrSourceStart) {
		t.Fatalf("Add A error = %v, want ErrSourceStart", err)
	}
	if err := h.l.Add(name, localRef("/tmp/fails-late-c")); err != nil {
		t.Fatalf("Add C after A failed: %v", err)
	}
	h.assertConsistent(t)
}

func TestSourceLifecycle_DuplicateName_ErrSourceExists(t *testing.T) {
	h := newLifecycleHarness(t)
	ref := localRef("/tmp/dup")
	if err := h.l.Add("dup", ref); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	first := h.cfg.entry("dup")
	persists := h.cfg.persists

	if err := h.l.Add("dup", localRef("/tmp/dup-2")); !errors.Is(err, ErrSourceExists) {
		t.Fatalf("second Add error = %v, want ErrSourceExists", err)
	}
	if h.cfg.entry("dup") != first {
		t.Fatal("duplicate Add replaced the existing entry")
	}
	if h.cfg.persists != persists {
		t.Fatal("duplicate Add persisted the config")
	}
	if h.rec.adds != 1 {
		t.Fatalf("reconciler AddSource called %d times, want 1", h.rec.adds)
	}
	h.assertConsistent(t)
}

func TestSourceLifecycle_RemoveUnknown_ErrSourceNotFound(t *testing.T) {
	h := newLifecycleHarness(t)
	if err := h.l.Remove("nope"); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("Remove error = %v, want ErrSourceNotFound", err)
	}
	if h.cfg.persists != 0 {
		t.Fatal("Remove of an unknown name persisted the config")
	}
}

func TestSourceLifecycle_Remove_UnregistersSource(t *testing.T) {
	h := newLifecycleHarness(t)
	if err := h.l.Add("gone", localRef("/tmp/gone")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, ok := h.mgr.Get("gone"); !ok {
		t.Fatal("Add did not register the source")
	}
	if err := h.l.Remove("gone"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := h.mgr.Get("gone"); ok {
		t.Fatal("Remove left the stopped source registered")
	}
	h.assertConsistent(t)
}

func TestSourceLifecycle_PersistFailure_IsNotFatal(t *testing.T) {
	h := newLifecycleHarness(t)
	h.cfg.persistErr = errors.New("disk full")

	if err := h.l.Add("p", localRef("/tmp/p")); err != nil {
		t.Fatalf("Add with persist failure: %v", err)
	}
	h.assertConsistent(t)
	if err := h.l.Remove("p"); err != nil {
		t.Fatalf("Remove with persist failure: %v", err)
	}
	h.assertConsistent(t)
}

// After DELETE, the dev-mode endpoint must treat the name as unknown rather
// than toggling the stopped source left behind.
func TestApiRemoveSource_DevModeOnRemovedSourceIsNotFound(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Port: 8080}}
	cfg.Spec.Entries = map[string]*taskset.Entry{}
	srv, _, sourceMgr := newTestServerWithReconciler(t, cfg)
	h := srv.Handler()

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	path := filepath.Join(t.TempDir(), "taskset.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: dicode/v1\nkind: TaskSet\nspec:\n  entries: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := do(http.MethodPost, "/api/settings/sources", `{"name":"doomed","path":"`+path+`"}`); w.Code != http.StatusOK {
		t.Fatalf("add: %d %s", w.Code, w.Body.String())
	}
	if _, ok := sourceMgr.Get("doomed"); !ok {
		t.Fatal("added source not registered")
	}
	if w := do(http.MethodPatch, "/api/sources/doomed/dev", `{"enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("dev-mode on live source: %d %s", w.Code, w.Body.String())
	}

	if w := do(http.MethodDelete, "/api/settings/sources/doomed", ""); w.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	w := do(http.MethodPatch, "/api/sources/doomed/dev", `{"enabled":false}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not found") {
		t.Fatalf("dev-mode on removed source: %d %s, want 400 not found", w.Code, w.Body.String())
	}
}

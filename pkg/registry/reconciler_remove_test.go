package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
)

// ctxFakeSource records the context it was started with and optionally
// implements source.Remover via onRemove.
type ctxFakeSource struct {
	id string
	ch chan source.Event

	mu       sync.Mutex
	startCtx context.Context
}

func newCtxFakeSource(id string) *ctxFakeSource {
	return &ctxFakeSource{id: id, ch: make(chan source.Event, 16)}
}

func (f *ctxFakeSource) ID() string { return f.id }
func (f *ctxFakeSource) Start(ctx context.Context) (<-chan source.Event, error) {
	f.mu.Lock()
	f.startCtx = ctx
	f.mu.Unlock()
	return f.ch, nil
}
func (f *ctxFakeSource) Sync(context.Context) error { return nil }
func (f *ctxFakeSource) started() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCtx != nil
}
func (f *ctxFakeSource) ctx() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCtx
}

type removerSource struct {
	*ctxFakeSource
	onRemove func()
}

func (r *removerSource) Remove() { r.onRemove() }

// blockingStartSource blocks inside Start until its context is cancelled or
// unblock is closed; ch is pre-filled by the test.
type blockingStartSource struct {
	id      string
	ch      chan source.Event
	entered chan context.Context
	unblock chan struct{}
}

func newBlockingStartSource(id string) *blockingStartSource {
	return &blockingStartSource{
		id:      id,
		ch:      make(chan source.Event, 16),
		entered: make(chan context.Context, 1),
		unblock: make(chan struct{}),
	}
}

func (b *blockingStartSource) ID() string { return b.id }
func (b *blockingStartSource) Start(ctx context.Context) (<-chan source.Event, error) {
	b.entered <- ctx
	select {
	case <-ctx.Done():
	case <-b.unblock:
	}
	return b.ch, nil
}
func (b *blockingStartSource) Sync(context.Context) error { return nil }

func manualSpec(id string) *task.Spec {
	return &task.Spec{ID: id, Name: id, Trigger: task.TriggerConfig{Manual: true}}
}

func waitRegistered(t *testing.T, reg *Registry, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.Get(id); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %q never registered", id)
}

type unregisterLog struct {
	mu  sync.Mutex
	ids []string
}

func (u *unregisterLog) add(id string) {
	u.mu.Lock()
	u.ids = append(u.ids, id)
	u.mu.Unlock()
}

func (u *unregisterLog) snapshot() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.ids...)
}

func startReconciler(t *testing.T, rec *Reconciler) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()
	select {
	case <-rec.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("reconciler never became ready")
	}
	return cancel, done
}

func TestReconciler_RemoveSource_DeregistersItsTasks(t *testing.T) {
	reg, rec := newTestReconciler(t)
	var unreg unregisterLog
	rec.OnUnregister = unreg.add
	startReconciler(t, rec)

	a := newCtxFakeSource("src-a")
	b := newCtxFakeSource("src-b")
	for _, s := range []*ctxFakeSource{a, b} {
		if err := rec.AddSource(s); err != nil {
			t.Fatalf("AddSource(%s): %v", s.id, err)
		}
	}
	a.ch <- source.Event{Kind: source.EventAdded, TaskID: "a/one", Kinded: manualSpec("a/one"), Source: "src-a"}
	a.ch <- source.Event{Kind: source.EventAdded, TaskID: "a/two", Kinded: manualSpec("a/two"), Source: "src-a"}
	b.ch <- source.Event{Kind: source.EventAdded, TaskID: "b/one", Kinded: manualSpec("b/one"), Source: "src-b"}
	for _, id := range []string{"a/one", "a/two", "b/one"} {
		waitRegistered(t, reg, id)
	}

	rec.RemoveSource("src-a")

	for _, id := range []string{"a/one", "a/two"} {
		if _, ok := reg.Get(id); ok {
			t.Errorf("task %q still registered after its source was removed", id)
		}
	}
	if _, ok := reg.Get("b/one"); !ok {
		t.Error("task of another source was deregistered")
	}
	got := unreg.snapshot()
	if len(got) != 2 {
		t.Fatalf("OnUnregister calls = %v, want a/one and a/two", got)
	}
	if a.ctx().Err() == nil {
		t.Error("removed source's context not cancelled")
	}
}

func TestReconciler_RemoveSource_CallsRemoverAfterDeregistering(t *testing.T) {
	reg, rec := newTestReconciler(t)
	startReconciler(t, rec)

	removed := make(chan bool, 1)
	src := &removerSource{ctxFakeSource: newCtxFakeSource("src-r")}
	src.onRemove = func() {
		_, stillRegistered := reg.Get("r/task")
		removed <- stillRegistered
	}
	if err := rec.AddSource(src); err != nil {
		t.Fatal(err)
	}
	src.ch <- source.Event{Kind: source.EventAdded, TaskID: "r/task", Kinded: manualSpec("r/task"), Source: "src-r"}
	waitRegistered(t, reg, "r/task")

	rec.RemoveSource("src-r")
	select {
	case stillRegistered := <-removed:
		if stillRegistered {
			t.Fatal("Remove ran before the source's tasks were deregistered")
		}
	default:
		t.Fatal("RemoveSource returned without calling Remove")
	}
}

// Daemon shutdown cancels the run context; that must neither deregister
// tasks nor call Remove (dev clones survive a restart).
func TestReconciler_Shutdown_DoesNotDeregister(t *testing.T) {
	reg, rec := newTestReconciler(t)
	var unreg unregisterLog
	rec.OnUnregister = unreg.add
	cancel, done := startReconciler(t, rec)

	removeCalled := make(chan struct{}, 1)
	src := &removerSource{ctxFakeSource: newCtxFakeSource("src-s")}
	src.onRemove = func() { removeCalled <- struct{}{} }
	if err := rec.AddSource(src); err != nil {
		t.Fatal(err)
	}
	src.ch <- source.Event{Kind: source.EventAdded, TaskID: "s/task", Kinded: manualSpec("s/task"), Source: "src-s"}
	waitRegistered(t, reg, "s/task")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
	time.Sleep(50 * time.Millisecond)

	if _, ok := reg.Get("s/task"); !ok {
		t.Error("shutdown deregistered a task")
	}
	if got := unreg.snapshot(); len(got) != 0 {
		t.Errorf("OnUnregister called on shutdown: %v", got)
	}
	select {
	case <-removeCalled:
		t.Error("Remove called on shutdown")
	default:
	}
}

func TestReconciler_RemoveSource_CancelsInProgressStart(t *testing.T) {
	reg, rec := newTestReconciler(t)
	startReconciler(t, rec)

	src := newBlockingStartSource("src-slow")
	src.ch <- source.Event{Kind: source.EventAdded, TaskID: "slow/task", Kinded: manualSpec("slow/task"), Source: "src-slow"}
	addDone := make(chan error, 1)
	go func() { addDone <- rec.AddSource(src) }()
	startCtx := <-src.entered

	removeDone := make(chan struct{})
	go func() { rec.RemoveSource("src-slow"); close(removeDone) }()
	select {
	case <-startCtx.Done():
	case <-time.After(2 * time.Second):
		close(src.unblock)
		t.Fatal("RemoveSource did not cancel a source that was still starting")
	}
	if err := <-addDone; err != nil {
		t.Fatalf("AddSource after concurrent removal = %v, want nil", err)
	}
	<-removeDone

	time.Sleep(100 * time.Millisecond)
	if _, ok := reg.Get("slow/task"); ok {
		t.Fatal("a source removed while starting registered its task")
	}
	rec.mu.Lock()
	n := len(rec.cancels)
	rec.mu.Unlock()
	if n != 0 {
		t.Fatalf("rc.cancels has %d entries after removal, want 0", n)
	}
}

// An initially-configured source removed mid-start must not hold Ready open.
func TestReconciler_RemoveInitialSourceDuringStart_ReadyCloses(t *testing.T) {
	src := newBlockingStartSource("src-init")
	_, rec := newTestReconciler(t, src)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rec.Run(ctx) }()
	<-src.entered

	go rec.RemoveSource("src-init")
	select {
	case <-rec.Ready():
	case <-time.After(2 * time.Second):
		close(src.unblock)
		t.Fatal("Ready never closed after the initial source was removed mid-start")
	}
}

// A task queued for a missing provider must not register once its source is
// gone, even when the provider shows up later.
func TestReconciler_RemoveSource_DropsPendingRetries(t *testing.T) {
	reg, rec := newTestReconciler(t)
	startReconciler(t, rec)

	consumerSrc := newCtxFakeSource("src-consumer")
	providerSrc := newCtxFakeSource("src-provider")
	for _, s := range []*ctxFakeSource{consumerSrc, providerSrc} {
		if err := rec.AddSource(s); err != nil {
			t.Fatal(err)
		}
	}
	consumer := manualSpec("c/consumer")
	consumer.Permissions.Env = []task.EnvEntry{{Name: "TOKEN", From: "task:p/provider"}}
	consumerSrc.ch <- source.Event{Kind: source.EventAdded, TaskID: "c/consumer", Kinded: consumer, Source: "src-consumer"}

	deadline := time.Now().Add(2 * time.Second)
	for {
		rec.mu.Lock()
		_, queued := rec.pending["c/consumer"]
		rec.mu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("consumer never queued")
		}
		time.Sleep(5 * time.Millisecond)
	}

	rec.RemoveSource("src-consumer")
	providerSrc.ch <- source.Event{Kind: source.EventAdded, TaskID: "p/provider", Kinded: manualSpec("p/provider"), Source: "src-provider"}
	waitRegistered(t, reg, "p/provider")
	time.Sleep(100 * time.Millisecond)

	if _, ok := reg.Get("c/consumer"); ok {
		t.Fatal("task of a removed source registered from the pending retry queue")
	}
}

// An event already in flight from a removed source run, including after a
// new source reuses the same ID, must be dropped.
func TestReconciler_RemoveSource_DropsLateEvents(t *testing.T) {
	reg, rec := newTestReconciler(t)
	startReconciler(t, rec)

	old := newCtxFakeSource("src-x")
	if err := rec.AddSource(old); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	oldGen := rec.cancels["src-x"].gen
	rec.mu.Unlock()
	rec.RemoveSource("src-x")

	fresh := newCtxFakeSource("src-x")
	if err := rec.AddSource(fresh); err != nil {
		t.Fatal(err)
	}
	rec.merged <- sourceEvent{
		Event: source.Event{Kind: source.EventAdded, TaskID: "x/stale", Kinded: manualSpec("x/stale"), Source: "src-x"},
		gen:   oldGen,
	}
	fresh.ch <- source.Event{Kind: source.EventAdded, TaskID: "x/fresh", Kinded: manualSpec("x/fresh"), Source: "src-x"}
	waitRegistered(t, reg, "x/fresh")

	if _, ok := reg.Get("x/stale"); ok {
		t.Fatal("late event from a removed source run re-registered a task")
	}
}

// A caller holding a removed source must not tear down a newer source that
// reuses its ID.
func TestReconciler_RemoveSourceIfCurrent_SparesNewerSameID(t *testing.T) {
	reg, rec := newTestReconciler(t)
	startReconciler(t, rec)

	old := newCtxFakeSource("src-x")
	if err := rec.AddSource(old); err != nil {
		t.Fatal(err)
	}
	rec.RemoveSource("src-x")

	fresh := newCtxFakeSource("src-x")
	if err := rec.AddSource(fresh); err != nil {
		t.Fatal(err)
	}
	fresh.ch <- source.Event{Kind: source.EventAdded, TaskID: "x/fresh", Kinded: manualSpec("x/fresh")}
	waitRegistered(t, reg, "x/fresh")

	rec.RemoveSourceIfCurrent(old)

	if fresh.ctx().Err() != nil {
		t.Fatal("stale removal cancelled the newer source")
	}
	if _, ok := reg.Get("x/fresh"); !ok {
		t.Fatal("stale removal deregistered the newer source's task")
	}

	rec.RemoveSourceIfCurrent(fresh)
	if _, ok := reg.Get("x/fresh"); ok {
		t.Fatal("RemoveSourceIfCurrent did not remove the current source")
	}
}

// stubbornStartSource blocks in Start until unblock closes, ignoring ctx.
type stubbornStartSource struct {
	*blockingStartSource
	removed chan struct{}
}

func (b *stubbornStartSource) Start(ctx context.Context) (<-chan source.Event, error) {
	b.entered <- ctx
	<-b.unblock
	return b.ch, nil
}
func (b *stubbornStartSource) Remove() { close(b.removed) }

// Remove must not run while the source's Start is still in progress.
func TestReconciler_RemoveSource_WaitsForStartBeforeRemove(t *testing.T) {
	_, rec := newTestReconciler(t)
	startReconciler(t, rec)

	src := &stubbornStartSource{blockingStartSource: newBlockingStartSource("src-stubborn"), removed: make(chan struct{})}
	addDone := make(chan error, 1)
	go func() { addDone <- rec.AddSource(src) }()
	<-src.entered

	removeDone := make(chan struct{})
	go func() { rec.RemoveSource("src-stubborn"); close(removeDone) }()
	select {
	case <-src.removed:
		close(src.unblock)
		t.Fatal("Remove ran while Start was still in progress")
	case <-time.After(100 * time.Millisecond):
	}
	close(src.unblock)
	select {
	case <-removeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("RemoveSource never returned after Start finished")
	}
	select {
	case <-src.removed:
	default:
		t.Fatal("Remove not called")
	}
	if err := <-addDone; err != nil {
		t.Fatalf("AddSource = %v", err)
	}
}

// The forwarder stamps every event with the producing source's ID.
func TestReconciler_ForwarderStampsSourceID(t *testing.T) {
	reg, rec := newTestReconciler(t)
	startReconciler(t, rec)

	src := newCtxFakeSource("src-stamp")
	if err := rec.AddSource(src); err != nil {
		t.Fatal(err)
	}
	src.ch <- source.Event{Kind: source.EventAdded, TaskID: "s/task", Kinded: manualSpec("s/task"), Source: "something-else"}
	waitRegistered(t, reg, "s/task")
	rec.RemoveSource("src-stamp")
	if _, ok := reg.Get("s/task"); ok {
		t.Fatal("task not deregistered with its source")
	}
}

// A source ID stays claimed until its teardown, Remove included, finishes.
func TestReconciler_RemoveSource_IDBusyUntilTeardownDone(t *testing.T) {
	_, rec := newTestReconciler(t)
	startReconciler(t, rec)

	release := make(chan struct{})
	inRemove := make(chan struct{})
	old := &removerSource{ctxFakeSource: newCtxFakeSource("src-busy")}
	old.onRemove = func() { close(inRemove); <-release }
	if err := rec.AddSource(old); err != nil {
		t.Fatal(err)
	}
	removeDone := make(chan struct{})
	go func() { rec.RemoveSource("src-busy"); close(removeDone) }()
	<-inRemove

	if err := rec.AddSource(newCtxFakeSource("src-busy")); !errors.Is(err, ErrDuplicateSource) {
		close(release)
		t.Fatalf("AddSource during teardown = %v, want ErrDuplicateSource", err)
	}
	close(release)
	<-removeDone
	if err := rec.AddSource(newCtxFakeSource("src-busy")); err != nil {
		t.Fatalf("AddSource after teardown = %v", err)
	}
}

// A second RemoveSource for a source already being removed returns only once
// the first teardown has finished.
func TestReconciler_ConcurrentRemoveSource_WaitsForTeardown(t *testing.T) {
	reg, rec := newTestReconciler(t)
	startReconciler(t, rec)

	release := make(chan struct{})
	inRemove := make(chan struct{})
	src := &removerSource{ctxFakeSource: newCtxFakeSource("src-twice")}
	src.onRemove = func() { close(inRemove); <-release }
	if err := rec.AddSource(src); err != nil {
		t.Fatal(err)
	}
	src.ch <- source.Event{Kind: source.EventAdded, TaskID: "t/task", Kinded: manualSpec("t/task")}
	waitRegistered(t, reg, "t/task")

	go rec.RemoveSource("src-twice")
	<-inRemove

	second := make(chan struct{})
	go func() { rec.RemoveSource("src-twice"); close(second) }()
	select {
	case <-second:
		close(release)
		t.Fatal("second RemoveSource returned before the first teardown finished")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("second RemoveSource never returned")
	}
	if rec.Running("src-twice") != nil {
		t.Fatal("source still registered after teardown")
	}
}

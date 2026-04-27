package envresolve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
)

// fakeRegistry stores task specs by id.
type fakeRegistry struct{ specs map[string]*task.Spec }

func (f *fakeRegistry) Get(id string) (*task.Spec, bool) {
	s, ok := f.specs[id]
	return s, ok
}

// fakeRunner records calls and returns canned values.
type fakeRunner struct {
	calls    int
	lastReqs []ProviderRequest
	values   map[string]string
	err      error
}

func (f *fakeRunner) Run(ctx context.Context, providerID string, reqs []ProviderRequest) (*ProviderResult, error) {
	f.calls++
	f.lastReqs = reqs
	if f.err != nil {
		return nil, f.err
	}
	return &ProviderResult{Values: f.values}, nil
}

func newSpec(id string, env []task.EnvEntry) *task.Spec {
	return &task.Spec{
		ID:          id,
		Name:        id,
		Permissions: task.Permissions{Env: env},
	}
}

func newProviderSpec(id string, ttl time.Duration) *task.Spec {
	return &task.Spec{
		ID:       id,
		Name:     id,
		Provider: &task.ProviderConfig{CacheTTL: ttl},
	}
}

func TestResolve_BatchesProviderSpawnPerLaunch(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]*task.Spec{
		"doppler": newProviderSpec("doppler", 5*time.Minute),
	}}
	runner := &fakeRunner{values: map[string]string{
		"PG_URL":    "postgres://x",
		"REDIS_URL": "redis://y",
	}}
	r := New(reg, secrets.Chain{}, runner)
	r.Now = func() time.Time { return time.Unix(0, 0) }

	consumer := newSpec("consumer", []task.EnvEntry{
		{Name: "PG_URL", From: "task:doppler"},
		{Name: "REDIS_URL", From: "task:doppler"},
	})
	got, err := r.Resolve(context.Background(), consumer)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("expected 1 spawn (batched), got %d", runner.calls)
	}
	if got.Env["PG_URL"] != "postgres://x" || got.Env["REDIS_URL"] != "redis://y" {
		t.Errorf("env = %#v", got.Env)
	}
	if got.Secrets["PG_URL"] != "postgres://x" {
		t.Errorf("PG_URL not flagged as secret: %#v", got.Secrets)
	}
}

func TestResolve_CacheHitSkipsSpawn(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]*task.Spec{
		"doppler": newProviderSpec("doppler", 5*time.Minute),
	}}
	runner := &fakeRunner{values: map[string]string{"PG_URL": "v1"}}
	r := New(reg, secrets.Chain{}, runner)
	r.Now = func() time.Time { return time.Unix(0, 0) }

	consumer := newSpec("consumer", []task.EnvEntry{
		{Name: "PG_URL", From: "task:doppler"},
	})

	if _, err := r.Resolve(context.Background(), consumer); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if _, err := r.Resolve(context.Background(), consumer); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("expected exactly one spawn (second hit cache), got %d", runner.calls)
	}
}

func TestResolve_RequiredKeyMissing(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]*task.Spec{
		"doppler": newProviderSpec("doppler", 0),
	}}
	runner := &fakeRunner{values: map[string]string{}} // returns empty map
	r := New(reg, secrets.Chain{}, runner)
	r.Now = func() time.Time { return time.Unix(0, 0) }

	consumer := newSpec("consumer", []task.EnvEntry{
		{Name: "PG_URL", From: "task:doppler"},
	})
	_, err := r.Resolve(context.Background(), consumer)
	var miss *ErrRequiredSecretMissing
	if !errors.As(err, &miss) {
		t.Fatalf("expected ErrRequiredSecretMissing, got %T %v", err, err)
	}
	if miss.Key != "PG_URL" || miss.ProviderID != "doppler" {
		t.Errorf("err = %+v", miss)
	}
}

func TestResolve_OptionalKeyMissingIsEmpty(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]*task.Spec{
		"doppler": newProviderSpec("doppler", 0),
	}}
	runner := &fakeRunner{values: map[string]string{}}
	r := New(reg, secrets.Chain{}, runner)
	r.Now = func() time.Time { return time.Unix(0, 0) }

	consumer := newSpec("consumer", []task.EnvEntry{
		{Name: "OPTIONAL", From: "task:doppler", Optional: true},
	})
	got, err := r.Resolve(context.Background(), consumer)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Env["OPTIONAL"] != "" {
		t.Errorf("expected empty, got %q", got.Env["OPTIONAL"])
	}
}

func TestResolve_ProviderUnavailable(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]*task.Spec{
		"doppler": newProviderSpec("doppler", 0),
	}}
	runner := &fakeRunner{err: errors.New("spawn failed")}
	r := New(reg, secrets.Chain{}, runner)
	r.Now = func() time.Time { return time.Unix(0, 0) }

	consumer := newSpec("consumer", []task.EnvEntry{
		{Name: "PG_URL", From: "task:doppler"},
	})
	_, err := r.Resolve(context.Background(), consumer)
	var pu *ErrProviderUnavailable
	if !errors.As(err, &pu) {
		t.Fatalf("expected ErrProviderUnavailable, got %T %v", err, err)
	}
}

func TestResolve_BarePrefixIsHostEnv(t *testing.T) {
	t.Setenv("FOO_FROM_HOST", "hello")
	r := New(&fakeRegistry{}, secrets.Chain{}, nil)
	consumer := newSpec("consumer", []task.EnvEntry{
		{Name: "FOO", From: "FOO_FROM_HOST"},
		{Name: "BAR", From: "env:FOO_FROM_HOST"},
	})
	got, err := r.Resolve(context.Background(), consumer)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Env["FOO"] != "hello" || got.Env["BAR"] != "hello" {
		t.Errorf("env = %#v", got.Env)
	}
	if _, ok := got.Secrets["FOO"]; ok {
		t.Errorf("host-env values must NOT be flagged secret")
	}
}

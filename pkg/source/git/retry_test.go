package git

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/internal/gitops"
	gogittransport "github.com/go-git/go-git/v5/plumbing/transport"
	"go.uber.org/zap"
)

// Tests for the bounded retry wrapper around syncRepo.
//
// The production trySyncRepo path is exercised via the existing tests in
// git_test.go (which use a real file:// remote). These tests focus on the
// retry semantics: how many attempts are made, that auth-style failures
// short-circuit, and that the operation doesn't burn the entire MaxElapsed
// budget on a permanent error.
//
// Each test injects a mock syncRepoOp on a freshly-constructed GitSource
// so the real go-git code path never runs.

func newTestGitSource(t *testing.T, op syncRepoFn) *GitSource {
	t.Helper()
	gs, err := New(t.TempDir(), "https://example.invalid/repo.git", "main", time.Second, "", "", zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gs.syncRepoOp = op
	return gs
}

// TestSyncRepo_TransientErrorRetries verifies that a transient error
// triggers at least one retry and eventually succeeds when the operation
// recovers.
func TestSyncRepo_TransientErrorRetries(t *testing.T) {
	var calls atomic.Int32
	gs := newTestGitSource(t, func(ctx context.Context) error {
		n := calls.Add(1)
		if n < 3 {
			return errors.New("connection reset by peer")
		}
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := gs.syncRepo(ctx); err != nil {
		t.Fatalf("syncRepo: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("call count = %d; want 3 (2 retries before success)", got)
	}
}

// TestSyncRepo_AuthErrorIsPermanent verifies that an authentication
// failure surfaces immediately without retrying. Auth errors are not
// retried so a misconfigured token doesn't burn the
// 30-second retry budget on every poll tick — and so the operator gets a
// fast, clear failure signal in the logs.
func TestSyncRepo_AuthErrorIsPermanent(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"AuthenticationRequired", gogittransport.ErrAuthenticationRequired},
		{"AuthorizationFailed", gogittransport.ErrAuthorizationFailed},
		{"InvalidAuthMethod", gogittransport.ErrInvalidAuthMethod},
		{"RepositoryNotFound", gogittransport.ErrRepositoryNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			// Wrap the sentinel to mirror how the production code returns
			// it (fmt.Errorf("clone: %w", err)) — isPermanentGitError must
			// still recognise the error after wrapping.
			wrapped := fmt.Errorf("clone: %w", tc.err)
			gs := newTestGitSource(t, func(ctx context.Context) error {
				calls.Add(1)
				return wrapped
			})

			start := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			err := gs.syncRepo(ctx)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("err = %v; want errors.Is(%v) == true", err, tc.err)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("call count = %d; want 1 (auth errors must not retry)", got)
			}
			// Should fail fast — well under the 30s MaxElapsedTime budget.
			if elapsed > 5*time.Second {
				t.Errorf("permanent error took %v to surface; want <5s", elapsed)
			}
		})
	}
}

// TestSyncRepo_BoundedByMaxElapsed verifies that a permanently failing
// transient error eventually gives up — we don't loop forever, so a broken
// upstream doesn't pin the poll goroutine in retry purgatory.
func TestSyncRepo_BoundedByMaxElapsed(t *testing.T) {
	var calls atomic.Int32
	gs := newTestGitSource(t, func(ctx context.Context) error {
		calls.Add(1)
		return errors.New("temporary failure in name resolution")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*cloneRetryMaxElapsed)
	defer cancel()

	start := time.Now()
	err := gs.syncRepo(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after retry budget exhausted")
	}
	// Allow some slack: backoff jitter + scheduler can push us slightly
	// past MaxElapsedTime. Upper bound is 2x to catch a runaway loop.
	if elapsed > 2*cloneRetryMaxElapsed {
		t.Errorf("retry took %v; expected <= %v", elapsed, 2*cloneRetryMaxElapsed)
	}
	// Lower bound is 2 attempts (not a higher number) because backoff jitter
	// can push the first wait to ~750ms and the second to ~1.5s, leaving
	// only ~3 attempts in the 30s budget at the worst end of the band.
	// If InitialInterval is ever bumped meaningfully, this floor will need
	// to drop — but never below 2, otherwise the test no longer proves
	// retries happened at all.
	if calls.Load() < 2 {
		t.Errorf("expected at least 2 attempts; got %d", calls.Load())
	}
}

// TestSyncRepo_ContextCancelStopsRetry verifies that cancelling ctx
// during retry exits promptly rather than waiting for the next backoff
// interval. Important for graceful shutdown — a daemon stop shouldn't
// have to wait for a retry cycle to finish.
func TestSyncRepo_ContextCancelStopsRetry(t *testing.T) {
	var calls atomic.Int32
	gs := newTestGitSource(t, func(ctx context.Context) error {
		calls.Add(1)
		return errors.New("network unreachable")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := gs.syncRepo(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error when ctx cancelled mid-retry")
	}
	// Shouldn't take much longer than the ctx deadline; certainly nowhere
	// near the 30s MaxElapsedTime.
	if elapsed > 5*time.Second {
		t.Errorf("ctx-cancelled retry took %v; expected ~ctx deadline", elapsed)
	}
}

// TestSyncRepo_SSRFBlockedHostDoesNotRetry is the core regression test
// for #510: before isPermanentGitError recognised gitops.ErrBlockedHost /
// gitops.ErrNoRemoteHost, a deterministic, zero-I/O SSRF rejection from
// ValidateRemoteHost was retried for the full ~30s cloneRetryMaxElapsed
// window on every poll tick — and since GitSource.Start calls syncRepo
// synchronously and sources start up sequentially in the reconciler, a
// single SSRF-blocked source stalled the whole initial sync by ~30s. This
// proves the op is now called exactly once and returns promptly.
func TestSyncRepo_SSRFBlockedHostDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	blockedErr := gitops.ValidateRemoteHost("ssh://git@127.0.0.1/org/repo.git")
	if blockedErr == nil {
		t.Fatal("ValidateRemoteHost did not reject the malicious host; test fixture is broken")
	}
	gs := newTestGitSource(t, func(ctx context.Context) error {
		calls.Add(1)
		return blockedErr
	})

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := gs.syncRepo(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, gitops.ErrBlockedHost) {
		t.Errorf("err = %v; want errors.Is(gitops.ErrBlockedHost) == true", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("call count = %d; want 1 (SSRF-blocked host must not retry)", got)
	}
	if elapsed > 5*time.Second {
		t.Errorf("SSRF-blocked error took %v to surface; want <5s (regression: was burning the full %v retry budget)", elapsed, cloneRetryMaxElapsed)
	}
}

// TestIsPermanentGitError_PermanentSentinels exercises the classifier
// directly — the Retry test above goes through the full backoff machinery
// which is slower; this catches regressions in the sentinel list itself.
func TestIsPermanentGitError_PermanentSentinels(t *testing.T) {
	permanent := []error{
		gogittransport.ErrAuthenticationRequired,
		gogittransport.ErrAuthorizationFailed,
		gogittransport.ErrInvalidAuthMethod,
		gogittransport.ErrRepositoryNotFound,
		fmt.Errorf("pull: %w", gogittransport.ErrAuthenticationRequired),
		fmt.Errorf("clone: %w", gogittransport.ErrRepositoryNotFound),
		// Regression coverage for #510: the real error shape produced by
		// gitops.ValidateRemoteHost (wired into CloneAtRef for #489) must
		// also be classified as permanent, otherwise a single SSRF-blocked
		// source burns the full ~30s retry budget on every poll tick.
		gitops.ValidateRemoteHost("ssh://git@127.0.0.1/org/repo.git"),
		gitops.ValidateRemoteHost("file:///etc/passwd"),
	}
	for _, err := range permanent {
		if !isPermanentGitError(err) {
			t.Errorf("isPermanentGitError(%v) = false; want true", err)
		}
	}

	transient := []error{
		errors.New("connection refused"),
		errors.New("i/o timeout"),
		errors.New("EOF"),
		fmt.Errorf("clone: %w", errors.New("503 service unavailable")),
		nil,
	}
	for _, err := range transient {
		if isPermanentGitError(err) {
			t.Errorf("isPermanentGitError(%v) = true; want false", err)
		}
	}
}

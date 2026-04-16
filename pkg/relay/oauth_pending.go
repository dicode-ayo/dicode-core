package relay

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DefaultPendingTTL bounds how long the daemon remembers an outstanding OAuth
// flow. The relay broker expires its own session at 5 minutes; we add a small
// grace window so an in-flight callback can still be correlated by session id
// after the broker has already forwarded the encrypted delivery.
const DefaultPendingTTL = 6 * time.Minute

// ErrPendingNotFound is returned when a session id is not (or no longer)
// tracked in the PendingSessions store.
var ErrPendingNotFound = errors.New("oauth: pending session not found")

// PendingSessions is a TTL-bounded in-memory map of OAuth flows that have
// been initiated but not yet completed. Its purpose is to bind each incoming
// token delivery on /hooks/oauth-complete to an AuthRequest the daemon
// actually issued — without this check, dicode.oauth.store_token would
// accept any envelope that happened to be encrypted to the daemon's public
// key, handing a malicious caller a chosen-salt oracle on the identity key.
//
// The store is intentionally non-persistent. A daemon restart invalidates
// outstanding flows; the user re-initiates. The relay broker's own session
// TTL is the same order of magnitude, so persistence would buy little.
type PendingSessions struct {
	mu      sync.Mutex
	byID    map[string]*AuthRequest
	ttl     time.Duration
	nowFunc func() time.Time
}

// NewPendingSessions creates an empty store with DefaultPendingTTL.
func NewPendingSessions() *PendingSessions {
	return &PendingSessions{
		byID:    make(map[string]*AuthRequest),
		ttl:     DefaultPendingTTL,
		nowFunc: time.Now,
	}
}

// Add registers a freshly-issued AuthRequest. The caller is responsible for
// making the session id unique (uuid.NewString in BuildAuthURL guarantees it
// in practice). Existing entries under the same id are overwritten.
func (s *PendingSessions) Add(req *AuthRequest) {
	if req == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[req.SessionID] = req
}

// Take atomically returns the AuthRequest for a given session id and removes
// it from the store. The caller must hold exclusive access to the returned
// struct — it will not be returned to anyone else. If no matching entry
// exists (or the entry has expired) Take returns ErrPendingNotFound.
//
// Take is the correct primitive for /hooks/oauth-complete handling: it
// enforces single-use binding of a delivery to a request the daemon
// actually issued. Evicting on read (even if downstream decrypt/parse/
// store later fails) guarantees that a token-delivery envelope cannot be
// replayed to trigger repeated decryptions of the same ciphertext.
func (s *PendingSessions) Take(sessionID string) (*AuthRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.byID[sessionID]
	if !ok {
		return nil, ErrPendingNotFound
	}
	delete(s.byID, sessionID)
	if s.nowFunc().Sub(time.Unix(req.Timestamp, 0)) > s.ttl {
		return nil, ErrPendingNotFound
	}
	return req, nil
}

// SweepExpired removes any sessions whose issue timestamp is older than ttl.
// Intended for a background ticker, but correctness does not depend on it —
// Take performs a lazy expiry check on every read.
func (s *PendingSessions) SweepExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.nowFunc().Add(-s.ttl)
	removed := 0
	for id, req := range s.byID {
		if time.Unix(req.Timestamp, 0).Before(cutoff) {
			delete(s.byID, id)
			removed++
		}
	}
	return removed
}

// Len reports the number of currently tracked sessions. Test-only convenience.
func (s *PendingSessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

// StartSweep runs SweepExpired on a ticker until ctx is cancelled.
// Safe to call from a background goroutine at daemon startup; the method
// returns when the context is done.
func (s *PendingSessions) StartSweep(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SweepExpired()
		}
	}
}

// Package envresolve resolves task env entries by walking permissions.env
// and dispatching to host env, secrets store, or provider tasks. Used by
// the Deno and Python runtimes before spawning the consumer process.
package envresolve

import (
	"sync"
	"time"
)

// cacheKey indexes a single resolved secret value.
type cacheKey struct {
	providerID string
	secretName string
}

type cacheEntry struct {
	value        string
	providerHash string
	expiresAt    time.Time
}

// cache is an in-memory TTL store for provider-task results. Not persisted:
// daemon restart re-fetches everything (issue #119: not worth the encryption
// complexity for cached upstream values).
//
// Concurrent access is guarded by a single RWMutex — provider hits are rare
// (per consumer launch) compared to in-process IPC, so contention is fine.
type cache struct {
	mu sync.RWMutex
	m  map[cacheKey]cacheEntry
}

func newCache() *cache {
	return &cache{m: make(map[cacheKey]cacheEntry)}
}

// get returns the cached value if (a) the entry exists, (b) the provider's
// content hash matches the stored one (otherwise the entry is purged), and
// (c) the TTL has not expired. now is the caller-supplied wall-clock so
// tests can drive the timeline deterministically.
func (c *cache) get(providerID, secretName, providerHash string, now time.Time) (string, bool) {
	k := cacheKey{providerID, secretName}

	c.mu.RLock()
	e, ok := c.m[k]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if e.providerHash != providerHash {
		// Content changed — purge ALL entries for this provider, not just
		// the one we just looked at. A new task hash means the operator
		// edited the provider task; old cached values from the previous
		// version may have been written under a different upstream policy.
		c.bustProvider(providerID)
		return "", false
	}
	if !now.Before(e.expiresAt) {
		return "", false
	}
	return e.value, true
}

// put writes a value with a TTL. ttl=0 is a no-op so providers can declare
// "never cache" by omitting cache_ttl from their task.yaml.
func (c *cache) put(providerID, secretName, providerHash, value string, ttl time.Duration, now time.Time) {
	if ttl <= 0 {
		return
	}
	k := cacheKey{providerID, secretName}
	c.mu.Lock()
	c.m[k] = cacheEntry{value: value, providerHash: providerHash, expiresAt: now.Add(ttl)}
	c.mu.Unlock()
}

// bustProvider drops every cached entry for providerID. Called on
// content-hash mismatch (see get) and exposed for the reconciler to call
// on EventUpdated/EventRemoved if needed in a follow-up.
func (c *cache) bustProvider(providerID string) {
	c.mu.Lock()
	for k := range c.m {
		if k.providerID == providerID {
			delete(c.m, k)
		}
	}
	c.mu.Unlock()
}

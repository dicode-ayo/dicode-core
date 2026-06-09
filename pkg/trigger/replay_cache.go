package trigger

import (
	"sync"
	"time"
)

type replayCacheEntry struct {
	digest string
	seenAt time.Time
}

// replayCache is a bounded, TTL-evicting nonce cache that rejects duplicate
// webhook bodies. Keyed on the hex-encoded HMAC digest. Safe for concurrent use.
type replayCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	entries    map[string]time.Time
	order      []replayCacheEntry
}

const defaultReplayCacheMax = 10_000

func newReplayCache(ttl time.Duration) *replayCache {
	return &replayCache{
		ttl:        ttl,
		maxEntries: defaultReplayCacheMax,
		entries:    make(map[string]time.Time),
	}
}

// seen returns true if the digest has been seen within the TTL window.
// If not seen, it records the digest and returns false.
func (c *replayCache) seen(digest string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	c.evictExpired(now)

	if t, ok := c.entries[digest]; ok && now.Sub(t) < c.ttl {
		return true
	}

	for len(c.entries) >= c.maxEntries && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest.digest)
	}

	c.entries[digest] = now
	c.order = append(c.order, replayCacheEntry{digest: digest, seenAt: now})
	return false
}

func (c *replayCache) evictExpired(now time.Time) {
	cutoff := now.Add(-c.ttl)
	i := 0
	for i < len(c.order) && c.order[i].seenAt.Before(cutoff) {
		delete(c.entries, c.order[i].digest)
		i++
	}
	if i > 0 {
		c.order = c.order[i:]
	}
}

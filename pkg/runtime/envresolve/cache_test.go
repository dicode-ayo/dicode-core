package envresolve

import (
	"testing"
	"time"
)

func TestCache_HitMissTTL(t *testing.T) {
	now := time.Now()
	c := newCache()

	// Miss on empty cache.
	if _, ok := c.get("doppler", "PG_URL", "hashA", now); ok {
		t.Fatalf("expected miss on empty cache")
	}

	// Put + hit within TTL.
	c.put("doppler", "PG_URL", "hashA", "postgres://x", 5*time.Second, now)
	if v, ok := c.get("doppler", "PG_URL", "hashA", now.Add(2*time.Second)); !ok || v != "postgres://x" {
		t.Fatalf("expected hit, got (%q, %v)", v, ok)
	}

	// Expire after TTL.
	if _, ok := c.get("doppler", "PG_URL", "hashA", now.Add(6*time.Second)); ok {
		t.Fatalf("expected expiry after TTL")
	}
}

func TestCache_BustOnHashChange(t *testing.T) {
	now := time.Now()
	c := newCache()
	c.put("doppler", "PG_URL", "hashA", "v1", time.Minute, now)

	// Same provider, different hash → miss (and old entry purged).
	if _, ok := c.get("doppler", "PG_URL", "hashB", now); ok {
		t.Fatalf("expected miss after content-hash change")
	}
	// Original key (hashA) also gone.
	if _, ok := c.get("doppler", "PG_URL", "hashA", now); ok {
		t.Fatalf("expected old hash entries purged")
	}
}

func TestCache_TTLZeroDisablesCaching(t *testing.T) {
	now := time.Now()
	c := newCache()
	c.put("doppler", "PG_URL", "hashA", "v1", 0, now)
	if _, ok := c.get("doppler", "PG_URL", "hashA", now); ok {
		t.Fatalf("ttl=0 must not cache")
	}
}

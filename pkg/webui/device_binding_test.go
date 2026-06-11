package webui

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
)

// issueRow inserts a device token directly so tests control created_at, ip and
// ua_family. Returns the raw token to present on renewal.
func issueRow(t *testing.T, d db.DB, ip string, fam *string, createdAgo time.Duration) string {
	t.Helper()
	raw, err := randomToken()
	if err != nil {
		t.Fatalf("randomToken: %v", err)
	}
	created := time.Now().Add(-createdAgo).Unix()
	exp := time.Now().Add(deviceTTL).Unix()
	if err := d.Exec(context.Background(),
		`INSERT INTO sessions (id, token_hash, kind, label, ip, ua_family, created_at, last_seen, expires_at)
		 VALUES (?, ?, 'device', 'test', ?, ?, ?, ?, ?)`,
		"id-"+raw[:8], hashToken(raw), ip, fam, created, created, exp,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return raw
}

func strptr(s string) *string { return &s }

func newTestStore(t *testing.T) (*dbSessionStore, db.DB) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return newDBSessionStore(d, nil), d
}

const chromeMac = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
const firefoxMac = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:126.0) Gecko/20100101 Firefox/126.0"

// Within-rotation-window: no new token, just last_seen refresh.
const fresh = time.Minute

func TestDeviceBinding_StrictSameSubnetPasses(t *testing.T) {
	store, d := newTestStore(t)
	ctx := context.Background()

	raw := issueRow(t, d, "203.0.113.10", strptr(uaFamily(chromeMac)), fresh)
	// Same /24, different host octet.
	_, ok, _ := store.renewFromDevice(ctx, raw, "203.0.113.99", chromeMac, bindingStrict)
	if !ok {
		t.Fatal("strict mode rejected a same-/24 renewal")
	}
}

func TestDeviceBinding_StrictCrossSubnetRejected(t *testing.T) {
	store, d := newTestStore(t)
	ctx := context.Background()

	raw := issueRow(t, d, "203.0.113.10", strptr(uaFamily(chromeMac)), fresh)
	_, ok, driftReject := store.renewFromDevice(ctx, raw, "198.51.100.10", chromeMac, bindingStrict)
	if ok {
		t.Fatal("strict mode allowed a cross-/24 renewal")
	}
	if !driftReject {
		t.Fatal("strict cross-subnet reject should set driftReject=true")
	}
	// H1: the offending row must be hard-revoked, not left replayable.
	devices, _ := store.listDevices(ctx)
	if len(devices) != 0 {
		t.Fatalf("strict drift must delete the device row, got %d rows", len(devices))
	}
	// Re-presenting the same cookie from the legit subnet must now also fail —
	// the row is gone.
	if _, ok2, _ := store.renewFromDevice(ctx, raw, "203.0.113.10", chromeMac, bindingStrict); ok2 {
		t.Fatal("revoked device token still accepted after strict-drift reject")
	}
}

func TestDeviceBinding_StrictUAFamilyMismatchRejected(t *testing.T) {
	store, d := newTestStore(t)
	ctx := context.Background()

	raw := issueRow(t, d, "203.0.113.10", strptr(uaFamily(chromeMac)), fresh)
	_, ok, driftReject := store.renewFromDevice(ctx, raw, "203.0.113.10", firefoxMac, bindingStrict)
	if ok {
		t.Fatal("strict mode allowed a UA-family mismatch")
	}
	if !driftReject {
		t.Fatal("strict UA-mismatch reject should set driftReject=true")
	}
}

func TestDeviceBinding_WarnCrossSubnetAllowedAndFlagged(t *testing.T) {
	store, d := newTestStore(t)
	ctx := context.Background()

	raw := issueRow(t, d, "203.0.113.10", strptr(uaFamily(chromeMac)), fresh)
	_, ok, _ := store.renewFromDevice(ctx, raw, "198.51.100.10", chromeMac, bindingWarn)
	if !ok {
		t.Fatal("warn mode rejected a renewal; should allow")
	}
	devices, err := store.listDevices(ctx)
	if err != nil {
		t.Fatalf("listDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	if !devices[0].Drift || devices[0].DriftReason != "ip" {
		t.Errorf("expected ip drift flag, got drift=%v reason=%q", devices[0].Drift, devices[0].DriftReason)
	}
}

func TestDeviceBinding_WarnUAFamilyDriftFlagged(t *testing.T) {
	store, d := newTestStore(t)
	ctx := context.Background()

	raw := issueRow(t, d, "203.0.113.10", strptr(uaFamily(chromeMac)), fresh)
	_, ok, _ := store.renewFromDevice(ctx, raw, "203.0.113.10", firefoxMac, bindingWarn)
	if !ok {
		t.Fatal("warn mode rejected a UA-drift renewal; should allow")
	}
	devices, _ := store.listDevices(ctx)
	if len(devices) != 1 || devices[0].DriftReason != "ua" {
		t.Fatalf("expected ua drift, got %+v", devices)
	}
}

// M3: warn-mode drift is sticky against the issue-time anchor. A renewal that is
// still off the issuing subnet keeps the flag; only a genuine return to the
// issuing subnet clears it.
func TestDeviceBinding_WarnDriftStickyUntilReturnToBaseline(t *testing.T) {
	store, d := newTestStore(t)
	ctx := context.Background()

	raw := issueRow(t, d, "203.0.113.10", strptr(uaFamily(chromeMac)), fresh)
	// Drift away from the issuing /24.
	if _, ok, _ := store.renewFromDevice(ctx, raw, "198.51.100.10", chromeMac, bindingWarn); !ok {
		t.Fatal("warn drift renewal rejected")
	}
	// Still off the issuing subnet (different from issue IP). The baseline must
	// not have re-anchored to 198.51.100.x, so this stays flagged.
	if _, ok, _ := store.renewFromDevice(ctx, raw, "198.51.100.20", chromeMac, bindingWarn); !ok {
		t.Fatal("warn renewal rejected")
	}
	devices, _ := store.listDevices(ctx)
	if len(devices) != 1 || !devices[0].Drift {
		t.Errorf("drift flag should stay set while still off the issuing subnet, got %+v", devices)
	}

	// Genuine return to the issuing /24 clears the flag.
	if _, ok, _ := store.renewFromDevice(ctx, raw, "203.0.113.55", chromeMac, bindingWarn); !ok {
		t.Fatal("return-to-baseline renewal rejected")
	}
	devices, _ = store.listDevices(ctx)
	if len(devices) != 1 || devices[0].Drift {
		t.Errorf("drift flag should clear on return to issuing subnet, got %+v", devices)
	}
}

// Off mode never rejects and never flags, even on a hard IP+UA change.
func TestDeviceBinding_OffNeverEnforces(t *testing.T) {
	store, d := newTestStore(t)
	ctx := context.Background()

	raw := issueRow(t, d, "203.0.113.10", strptr(uaFamily(chromeMac)), fresh)
	_, ok, _ := store.renewFromDevice(ctx, raw, "198.51.100.10", firefoxMac, bindingOff)
	if !ok {
		t.Fatal("off mode rejected a renewal")
	}
	devices, _ := store.listDevices(ctx)
	if len(devices) != 1 || devices[0].Drift {
		t.Errorf("off mode should not flag drift, got %+v", devices)
	}
}

// A legacy row with NULL ua_family must not be treated as a UA mismatch; the
// current family is backfilled on renewal.
func TestDeviceBinding_NullUAFamilyNotDrift(t *testing.T) {
	store, d := newTestStore(t)
	ctx := context.Background()

	raw := issueRow(t, d, "203.0.113.10", nil, fresh)
	if _, ok, _ := store.renewFromDevice(ctx, raw, "203.0.113.10", firefoxMac, bindingStrict); !ok {
		t.Fatal("strict mode rejected a NULL-ua_family (legacy) row")
	}
	// Family is now backfilled — a subsequent mismatch must be caught.
	if _, ok, _ := store.renewFromDevice(ctx, raw, "203.0.113.10", chromeMac, bindingStrict); ok {
		t.Fatal("strict mode allowed a mismatch after ua_family backfill")
	}
}

// IPv6 binding granularity is /48: same /48 passes, different /48 rejected.
func TestDeviceBinding_StrictIPv6Subnet(t *testing.T) {
	store, d := newTestStore(t)
	ctx := context.Background()

	raw := issueRow(t, d, "2001:db8:abcd:1::1", strptr(uaFamily(chromeMac)), fresh)
	if _, ok, _ := store.renewFromDevice(ctx, raw, "2001:db8:abcd:9::42", chromeMac, bindingStrict); !ok {
		t.Fatal("strict mode rejected a same-/48 IPv6 renewal")
	}
	raw2 := issueRow(t, d, "2001:db8:abcd:1::1", strptr(uaFamily(chromeMac)), fresh)
	if _, ok, _ := store.renewFromDevice(ctx, raw2, "2001:db8:ffff:1::1", chromeMac, bindingStrict); ok {
		t.Fatal("strict mode allowed a cross-/48 IPv6 renewal")
	}
}

// H2: drive the IP through the real clientIP path. A bracketed IPv6 RemoteAddr
// (host:port form, "[2001:db8::1]:443") must be normalized to a bare address so
// sameSubnet's /48 mask applies — otherwise IPv6 silently degrades to exact
// string compare and RFC 4941 privacy-address clients re-login constantly.
func TestDeviceBinding_StrictIPv6ThroughClientIP(t *testing.T) {
	store, d := newTestStore(t)
	ctx := context.Background()

	issueIP := clientIP(&http.Request{RemoteAddr: "[2001:db8:abcd:1::1]:51000"}, false)
	if issueIP != "2001:db8:abcd:1::1" {
		t.Fatalf("clientIP did not strip IPv6 brackets/port: got %q", issueIP)
	}
	raw := issueRow(t, d, issueIP, strptr(uaFamily(chromeMac)), fresh)

	// Same /48, different lower bits and a fresh ephemeral port (RFC 4941 churn).
	renewIP := clientIP(&http.Request{RemoteAddr: "[2001:db8:abcd:ffff::dead]:62000"}, false)
	if _, ok, _ := store.renewFromDevice(ctx, raw, renewIP, chromeMac, bindingStrict); !ok {
		t.Fatal("strict mode rejected a same-/48 IPv6 renewal via clientIP (bracket bug)")
	}

	// Trust-proxy XFF path with a bracketed IPv6 must normalize too.
	r := &http.Request{RemoteAddr: "10.0.0.1:80", Header: http.Header{}}
	r.Header.Set("X-Forwarded-For", "[2001:db8:abcd:2::9]:40000, 10.0.0.1")
	if got := clientIP(r, true); got != "2001:db8:abcd:2::9" {
		t.Fatalf("clientIP XFF did not normalize bracketed IPv6: got %q", got)
	}
}

// M1: an IPv4-mapped IPv6 address (::ffff:a.b.c.d) and the native IPv4 form of
// the same /24 must compare equal, not be rejected as a family mismatch.
func TestSameSubnet_IPv4MappedIPv6(t *testing.T) {
	if !sameSubnet("::ffff:203.0.113.10", "203.0.113.99") {
		t.Error("IPv4-mapped IPv6 and native IPv4 in the same /24 should match")
	}
	if sameSubnet("::ffff:203.0.113.10", "203.0.114.10") {
		t.Error("IPv4-mapped IPv6 across /24 boundary should not match")
	}
}

func TestUAFamily(t *testing.T) {
	cases := []struct {
		ua   string
		want string
	}{
		{chromeMac, "chrome/macos"},
		{firefoxMac, "firefox/macos"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0", "chrome/windows"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Safari/604.1", "safari/ios"},
		{"Mozilla/5.0 (Linux; Android 14) Chrome/120.0 Mobile", "chrome/android"},
		{"Mozilla/5.0 (Windows NT 10.0) Edg/120.0", "edge/windows"},
		{"", ""},
		{"curl/8.0", "other/other"},
	}
	for _, c := range cases {
		if got := uaFamily(c.ua); got != c.want {
			t.Errorf("uaFamily(%q) = %q, want %q", c.ua, got, c.want)
		}
	}
	// Chrome version churn must not change the family.
	if uaFamily(chromeMac) != uaFamily("Mozilla/5.0 (Macintosh; Intel Mac OS X 11_0) Chrome/200.0 Safari/537.36") {
		t.Error("uaFamily must be stable across Chrome version bumps")
	}
}

func TestSameSubnet(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"203.0.113.1", "203.0.113.254", true},
		{"203.0.113.1", "203.0.114.1", false},
		{"2001:db8:abcd:1::1", "2001:db8:abcd:ffff::9", true},
		{"2001:db8:abcd:1::1", "2001:db8:abce:1::1", false},
		{"203.0.113.1", "2001:db8::1", false}, // mixed family
		{"garbage", "garbage", true},          // unparseable falls back to equality
		{"garbage", "other", false},
	}
	for _, c := range cases {
		if got := sameSubnet(c.a, c.b); got != c.want {
			t.Errorf("sameSubnet(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

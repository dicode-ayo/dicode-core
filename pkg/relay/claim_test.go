package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	mrand "math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/db"
)

// newDeterministicIdentity returns a P-256 keypair derived from a fixed seed
// so assertions on the public UUID are stable across runs.
func newDeterministicIdentity(t *testing.T, seed int64) *Identity {
	t.Helper()
	// Deterministic reader for ecdsa.GenerateKey — sufficient for tests; never
	// use math/rand for real key material.
	r := mrand.New(mrand.NewSource(seed)) //nolint:gosec // deterministic test vector
	priv, err := ecdsa.GenerateKey(elliptic.P256(), &deterministicReader{r: r})
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &Identity{
		PrivateKey: priv,
		UUID:       deriveUUID(&priv.PublicKey),
	}
}

type deterministicReader struct{ r *mrand.Rand }

func (d *deterministicReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(d.r.Intn(256))
	}
	return len(p), nil
}

func TestBuildClaimSignature_PreimageShape(t *testing.T) {
	id := newDeterministicIdentity(t, 1)
	claimToken := "dcrct_test_token_0123456789"

	sigB64, err := BuildClaimSignature(id, claimToken)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatalf("decode base64 sig: %v", err)
	}

	// Canonical preimage from dicode-relay#14 clarification:
	//   preimage = utf8(claim_token) || hex_decode(uuid)
	uuidBytes, err := hex.DecodeString(id.UUID)
	if err != nil {
		t.Fatalf("decode uuid: %v", err)
	}
	if len(uuidBytes) != 32 {
		t.Fatalf("uuid must decode to 32 bytes, got %d", len(uuidBytes))
	}

	h := sha256.New()
	h.Write([]byte(claimToken))
	h.Write(uuidBytes)
	digest := h.Sum(nil)

	if !ecdsa.VerifyASN1(&id.PrivateKey.PublicKey, digest, sig) {
		t.Fatalf("signature did not verify against canonical preimage")
	}

	// Rejection: the ASCII-of-hex variant must NOT verify — that is the exact
	// mistake the #14 comment pins down.
	hBad := sha256.New()
	hBad.Write([]byte(claimToken))
	hBad.Write([]byte(id.UUID))
	if ecdsa.VerifyASN1(&id.PrivateKey.PublicKey, hBad.Sum(nil), sig) {
		t.Fatalf("signature verified against ASCII-hex variant; #14 ambiguity resurfaced")
	}
}

func TestBuildClaimSignature_RealKeyRoundTrip(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	id := &Identity{PrivateKey: priv, UUID: deriveUUID(&priv.PublicKey)}

	sigB64, err := BuildClaimSignature(id, "live-token")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sig, _ := base64.StdEncoding.DecodeString(sigB64)
	uuidBytes, _ := hex.DecodeString(id.UUID)
	h := sha256.Sum256(append([]byte("live-token"), uuidBytes...))
	if !ecdsa.VerifyASN1(&priv.PublicKey, h[:], sig) {
		t.Fatalf("round-trip verify failed")
	}
}

func TestBuildClaimSignature_ErrorCases(t *testing.T) {
	if _, err := BuildClaimSignature(nil, "tok"); err == nil {
		t.Fatalf("expected error for nil identity")
	}
	id := newDeterministicIdentity(t, 2)
	if _, err := BuildClaimSignature(id, ""); err == nil {
		t.Fatalf("expected error for empty claim token")
	}
	broken := &Identity{PrivateKey: id.PrivateKey, UUID: "not-hex"}
	if _, err := BuildClaimSignature(broken, "tok"); err == nil {
		t.Fatalf("expected error for non-hex uuid")
	}
	tooShort := &Identity{PrivateKey: id.PrivateKey, UUID: "deadbeef"}
	if _, err := BuildClaimSignature(tooShort, "tok"); err == nil {
		t.Fatalf("expected error for uuid not 32 bytes")
	}
	// Ensure the placeholder import stays used even if we later prune cases.
	_ = new(big.Int)
}

// fakeDB is a minimal in-memory db.DB implementation for claim tests.
type fakeDB struct {
	store map[string]string
}

func newFakeDB() *fakeDB                     { return &fakeDB{store: map[string]string{}} }
func (f *fakeDB) Ping(context.Context) error { return nil }
func (f *fakeDB) Close() error               { return nil }
func (f *fakeDB) Tx(ctx context.Context, fn func(tx db.DB) error) error {
	return fn(f)
}
func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) error {
	if !strings.Contains(sql, "INSERT OR REPLACE INTO kv") {
		return nil
	}
	if len(args) != 2 {
		return nil
	}
	key, _ := args[0].(string)
	val, _ := args[1].(string)
	f.store[key] = val
	return nil
}
func (f *fakeDB) Query(_ context.Context, sql string, args []any, fn func(db.Scanner) error) error {
	if !strings.Contains(sql, "SELECT value FROM kv WHERE key") {
		return fn(&fakeScanner{})
	}
	key, _ := args[0].(string)
	if v, ok := f.store[key]; ok {
		return fn(&fakeScanner{rows: []string{v}})
	}
	return fn(&fakeScanner{})
}

type fakeScanner struct {
	rows []string
	idx  int
}

func (s *fakeScanner) Next() bool {
	if s.idx >= len(s.rows) {
		return false
	}
	s.idx++
	return true
}
func (s *fakeScanner) Scan(dest ...any) error {
	if p, ok := dest[0].(*string); ok {
		*p = s.rows[s.idx-1]
	}
	return nil
}

func TestClaim_HappyPath(t *testing.T) {
	id := newDeterministicIdentity(t, 3)
	database := newFakeDB()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemons/claim" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing content-type")
		}
		var body ClaimRequest
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		if body.UUID != id.UUID {
			t.Errorf("uuid mismatch: got %q want %q", body.UUID, id.UUID)
		}
		if body.ClaimToken != "test-token-xyz" {
			t.Errorf("claim_token mismatch: %q", body.ClaimToken)
		}
		if body.Label != "my-laptop" {
			t.Errorf("label mismatch: %q", body.Label)
		}
		// Re-verify signature server-side so the full preimage contract is
		// exercised in both directions.
		sig, _ := base64.StdEncoding.DecodeString(body.Sig)
		uuidBytes, _ := hex.DecodeString(id.UUID)
		h := sha256.New()
		h.Write([]byte(body.ClaimToken))
		h.Write(uuidBytes)
		if !ecdsa.VerifyASN1(&id.PrivateKey.PublicKey, h.Sum(nil), sig) {
			t.Errorf("signature did not verify on the fake server")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ClaimResponse{OK: true, GithubLogin: "octocat"})
	}))
	defer server.Close()

	result, err := Claim(context.Background(), server.Client(), server.URL, id, "test-token-xyz", "my-laptop", database)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if result.GithubLogin != "octocat" {
		t.Fatalf("unexpected github login: %q", result.GithubLogin)
	}
	if database.store[kvKeyRelayClaimStatus] != "ok" {
		t.Fatalf("claim status not persisted: %+v", database.store)
	}
	if database.store[kvKeyRelayClaimUser] != "octocat" {
		t.Fatalf("claim user not persisted")
	}
	if database.store[kvKeyRelayClaimAt] == "" {
		t.Fatalf("claim_at not persisted")
	}

	status, err := LoadClaimStatus(context.Background(), database)
	if err != nil {
		t.Fatalf("load claim status: %v", err)
	}
	if !status.Linked() {
		t.Fatalf("expected linked state, got %+v", status)
	}
	if status.GithubLogin != "octocat" {
		t.Fatalf("load: github_login mismatch: %q", status.GithubLogin)
	}
}

func TestClaim_ErrorMapping(t *testing.T) {
	id := newDeterministicIdentity(t, 4)

	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"expired", http.StatusUnauthorized, `{"error":"token expired"}`, ErrClaimTokenInvalid},
		{"conflict", http.StatusConflict, `{"error":"already claimed"}`, ErrClaimConflict},
		{"forbidden", http.StatusForbidden, `{"error":"nope"}`, ErrClaimForbidden},
		{"badrequest", http.StatusBadRequest, `{"error":"bad sig"}`, ErrClaimBadRequest},
		{"server", http.StatusInternalServerError, `{"error":"boom"}`, ErrClaimServer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := Claim(context.Background(), srv.Client(), srv.URL, id, "tok", "", nil)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !errorsIs(err, tc.want) {
				t.Fatalf("error mapping mismatch: got %v want %v", err, tc.want)
			}
		})
	}
}

func TestClaim_NetworkError(t *testing.T) {
	id := newDeterministicIdentity(t, 5)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	_, err := Claim(context.Background(), &http.Client{}, srv.URL, id, "tok", "", nil)
	if err == nil {
		t.Fatalf("expected network error")
	}
	if !strings.Contains(err.Error(), "relay unreachable") {
		t.Fatalf("error should indicate unreachability: %v", err)
	}
}

func TestClaim_RejectsEmptyBaseURL(t *testing.T) {
	id := newDeterministicIdentity(t, 6)
	if _, err := Claim(context.Background(), nil, "", id, "tok", "", nil); err == nil {
		t.Fatalf("expected error for empty base url")
	}
}

// errorsIs unwraps with the stdlib unwrap convention; a local shim to avoid
// a second errors import cluttering the file.
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

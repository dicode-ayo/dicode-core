package relay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/db"
)

const (
	kvKeyRelayClaimStatus = "relay.claim_status"
	kvKeyRelayClaimUser   = "relay.claim_user"
	kvKeyRelayClaimAt     = "relay.claim_at"

	claimRequestTimeout = 20 * time.Second
)

// BuildClaimSignature signs the canonical claim preimage with the daemon's
// private key. The preimage is defined in dicode-relay issue #14:
//
//	preimage = utf8_bytes(claimToken) || hex_decode(identity.UUID)
//	message  = sha256(preimage)
//	sig      = ECDSA-P256-SHA256-DER(privkey, message)
//
// The uuid is the 32 raw bytes obtained by hex-decoding identity.UUID, NOT
// the ASCII bytes of the hex string — the server side in #14 must match.
// Returns the base64-encoded DER signature ready for JSON transport.
func BuildClaimSignature(identity *Identity, claimToken string) (string, error) {
	if identity == nil || identity.PrivateKey == nil {
		return "", errors.New("claim: nil identity")
	}
	if claimToken == "" {
		return "", errors.New("claim: empty claim token")
	}

	uuidBytes, err := hex.DecodeString(identity.UUID)
	if err != nil {
		return "", fmt.Errorf("claim: decode uuid: %w", err)
	}
	if len(uuidBytes) != 32 {
		return "", fmt.Errorf("claim: uuid must decode to 32 bytes, got %d", len(uuidBytes))
	}

	h := sha256.New()
	h.Write([]byte(claimToken))
	h.Write(uuidBytes)
	digest := h.Sum(nil)

	sig, err := ecdsa.SignASN1(rand.Reader, identity.PrivateKey, digest)
	if err != nil {
		return "", fmt.Errorf("claim: sign: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// ClaimRequest is the JSON body POSTed to the relay's /api/daemons/claim
// endpoint. Field names match the schema pinned in dicode-relay issue #14.
type ClaimRequest struct {
	ClaimToken string `json:"claim_token"`
	UUID       string `json:"uuid"`
	PubKey     string `json:"pubkey"`
	Sig        string `json:"sig"`
	Label      string `json:"label,omitempty"`
}

// ClaimResponse is the expected 2xx body from the relay.
type ClaimResponse struct {
	OK          bool   `json:"ok"`
	GithubLogin string `json:"github_login,omitempty"`
}

// ClaimErrorResponse is the JSON body returned by the relay on non-2xx.
type ClaimErrorResponse struct {
	Error string `json:"error,omitempty"`
	Hint  string `json:"hint,omitempty"`
}

// ClaimResult is the successful outcome of a claim call, suitable for
// persisting and displaying to the user.
type ClaimResult struct {
	UUID        string `json:"uuid"`
	GithubLogin string `json:"githubLogin,omitempty"`
}

// Claim performs the full daemon claim flow:
//  1. Build the ECDSA attestation over (claimToken || uuid)
//  2. POST ClaimRequest to {baseURL}/api/daemons/claim
//  3. On 2xx, persist KV flags so `dicode status` can surface the linked state
//
// It returns a friendly error message mapped from the HTTP status code on
// failure. The claim token is never logged.
func Claim(
	ctx context.Context,
	httpClient *http.Client,
	baseURL string,
	identity *Identity,
	claimToken, label string,
	database db.DB,
) (*ClaimResult, error) {
	if baseURL == "" {
		return nil, errors.New("claim: relay base URL is empty")
	}
	if identity == nil {
		return nil, errors.New("claim: nil identity")
	}

	sig, err := BuildClaimSignature(identity, claimToken)
	if err != nil {
		return nil, err
	}

	body := ClaimRequest{
		ClaimToken: claimToken,
		UUID:       identity.UUID,
		PubKey:     base64.StdEncoding.EncodeToString(identity.UncompressedPublicKey()),
		Sig:        sig,
		Label:      label,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("claim: marshal body: %w", err)
	}

	endpoint := strings.TrimRight(baseURL, "/") + "/api/daemons/claim"
	reqCtx, cancel := context.WithTimeout(ctx, claimRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("claim: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if httpClient == nil {
		httpClient = &http.Client{Timeout: claimRequestTimeout}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claim: relay unreachable: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var ok ClaimResponse
		_ = json.Unmarshal(respBody, &ok)
		result := &ClaimResult{UUID: identity.UUID, GithubLogin: ok.GithubLogin}
		if database != nil {
			if err := persistClaimFlags(ctx, database, result); err != nil {
				return result, fmt.Errorf("claim: persist kv: %w", err)
			}
		}
		return result, nil
	}

	return nil, mapClaimError(resp.StatusCode, respBody)
}

// mapClaimError turns a non-2xx HTTP response into a user-friendly error.
// Exported error sentinels let callers distinguish cases programmatically.
var (
	ErrClaimTokenInvalid = errors.New("claim token is invalid or expired")
	ErrClaimConflict     = errors.New("daemon is already claimed by a different user")
	ErrClaimForbidden    = errors.New("relay rejected the claim")
	ErrClaimBadRequest   = errors.New("relay rejected the claim request")
	ErrClaimServer       = errors.New("relay returned a server error")
)

func mapClaimError(status int, body []byte) error {
	var parsed ClaimErrorResponse
	_ = json.Unmarshal(body, &parsed)

	var base error
	switch status {
	case http.StatusUnauthorized:
		base = ErrClaimTokenInvalid
	case http.StatusConflict:
		base = ErrClaimConflict
	case http.StatusForbidden:
		base = ErrClaimForbidden
	case http.StatusBadRequest:
		base = ErrClaimBadRequest
	default:
		if status >= 500 {
			base = ErrClaimServer
		} else {
			base = fmt.Errorf("relay returned HTTP %d", status)
		}
	}

	if parsed.Error != "" {
		if parsed.Hint != "" {
			return fmt.Errorf("%w: %s (%s)", base, parsed.Error, parsed.Hint)
		}
		return fmt.Errorf("%w: %s", base, parsed.Error)
	}
	return base
}

func persistClaimFlags(ctx context.Context, database db.DB, result *ClaimResult) error {
	now := time.Now().UTC().Format(time.RFC3339)
	pairs := [][2]string{
		{kvKeyRelayClaimStatus, "ok"},
		{kvKeyRelayClaimUser, result.GithubLogin},
		{kvKeyRelayClaimAt, now},
	}
	for _, p := range pairs {
		if err := database.Exec(ctx,
			`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`,
			p[0], p[1],
		); err != nil {
			return err
		}
	}
	return nil
}

// LoadClaimStatus returns the persisted claim state, or an empty status if
// the daemon has never been claimed. Safe to call when the KV table is empty.
func LoadClaimStatus(ctx context.Context, database db.DB) (ClaimStatus, error) {
	var s ClaimStatus
	if database == nil {
		return s, nil
	}
	keys := []struct {
		k    string
		dest *string
	}{
		{kvKeyRelayClaimStatus, &s.Status},
		{kvKeyRelayClaimUser, &s.GithubLogin},
		{kvKeyRelayClaimAt, &s.ClaimedAt},
	}
	for _, item := range keys {
		var v string
		err := database.Query(ctx,
			`SELECT value FROM kv WHERE key = ?`,
			[]any{item.k},
			func(rows db.Scanner) error {
				if rows.Next() {
					return rows.Scan(&v)
				}
				return nil
			},
		)
		if err != nil {
			return s, err
		}
		*item.dest = v
	}
	return s, nil
}

// ClaimStatus summarises the daemon-side view of whether it has been claimed
// by a relay user account.
type ClaimStatus struct {
	Status      string `json:"status,omitempty"`      // "" | "ok"
	GithubLogin string `json:"githubLogin,omitempty"` // empty if unknown
	ClaimedAt   string `json:"claimedAt,omitempty"`   // RFC3339 or ""
}

// Linked reports whether the daemon has successfully been claimed.
func (c ClaimStatus) Linked() bool { return c.Status == "ok" }

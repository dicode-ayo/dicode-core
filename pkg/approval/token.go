package approval

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
)

// TokenTTL is how long a minted approve-link token stays redeemable.
const TokenTTL = 24 * time.Hour

// tokenPrefix marks dicode approval tokens so they are recognisable in
// notification URLs and secret scanners.
const tokenPrefix = "dcap_"

var (
	// ErrTokenInvalid covers unknown, malformed, and already-redeemed tokens.
	// Deliberately one error: a redemption attempt learns nothing about
	// whether the token ever existed.
	ErrTokenInvalid = errors.New("approval token invalid or already used")
	// ErrTokenExpired is returned for a token past its TTL. The row is
	// consumed on the failed redemption.
	ErrTokenExpired = errors.New("approval token expired")
)

// TokenInfo is the (task, hash) binding carried by an approval token.
type TokenInfo struct {
	TaskID string
	Hash   string
}

// TokenStore persists single-use, TTL'd approve-link tokens. Only the
// SHA-256 of a token is stored, so a database read cannot recover a
// redeemable token. Each token is bound to the exact (task_id, content_hash)
// pair it was minted for; redemption hands that binding to the gate, which
// refuses to approve any other version of the task.
type TokenStore struct {
	db  db.DB
	now func() time.Time // test hook
}

// NewTokenStore builds a TokenStore over the daemon database.
func NewTokenStore(d db.DB) *TokenStore {
	return &TokenStore{db: d, now: time.Now}
}

// Mint creates a single-use token bound to (taskID, hash), valid for
// TokenTTL, and returns the raw token. The raw value exists only in this
// return — the store keeps its SHA-256.
func (ts *TokenStore) Mint(ctx context.Context, taskID, hash string) (string, error) {
	if taskID == "" || hash == "" {
		return "", fmt.Errorf("approval token: task id and hash are required")
	}
	raw, err := ipc.NewSecret()
	if err != nil {
		return "", fmt.Errorf("approval token: %w", err)
	}
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	now := ts.now().UTC()

	// Opportunistic GC: expired rows are dead weight, drop them on mint.
	_ = ts.db.Exec(ctx, `DELETE FROM approval_tokens WHERE expires_at < ?`, now.Unix())

	if err := ts.db.Exec(ctx,
		`INSERT INTO approval_tokens (token_hash, task_id, content_hash, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		hashToken(token), taskID, hash, now.Unix(), now.Add(TokenTTL).Unix(),
	); err != nil {
		return "", fmt.Errorf("approval token: store: %w", err)
	}
	return token, nil
}

// Peek validates a token without consuming it. Used by the confirm page so a
// link prefetcher (mail client, chat unfurler) issuing GETs cannot burn the
// token or approve anything.
func (ts *TokenStore) Peek(ctx context.Context, token string) (TokenInfo, error) {
	info, expiresAt, err := ts.lookup(ctx, ts.db, token)
	if err != nil {
		return TokenInfo{}, err
	}
	if ts.now().UTC().Unix() > expiresAt {
		return TokenInfo{}, ErrTokenExpired
	}
	return info, nil
}

// Redeem consumes a token and returns its (task, hash) binding. The row is
// deleted before the result is returned — inside one transaction, so two
// concurrent redemptions cannot both succeed — and an expired token is
// consumed by the failed attempt.
func (ts *TokenStore) Redeem(ctx context.Context, token string) (TokenInfo, error) {
	var info TokenInfo
	var expiresAt int64
	err := ts.db.Tx(ctx, func(tx db.DB) error {
		var err error
		info, expiresAt, err = ts.lookup(ctx, tx, token)
		if err != nil {
			return err
		}
		return tx.Exec(ctx, `DELETE FROM approval_tokens WHERE token_hash = ?`, hashToken(token))
	})
	if err != nil {
		return TokenInfo{}, err
	}
	if ts.now().UTC().Unix() > expiresAt {
		return TokenInfo{}, ErrTokenExpired
	}
	return info, nil
}

// lookup resolves a raw token to its row via q (the base handle or a tx).
func (ts *TokenStore) lookup(ctx context.Context, q db.DB, token string) (TokenInfo, int64, error) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return TokenInfo{}, 0, ErrTokenInvalid
	}
	var info TokenInfo
	var expiresAt int64
	found := false
	err := q.Query(ctx,
		`SELECT task_id, content_hash, expires_at FROM approval_tokens WHERE token_hash = ?`,
		[]any{hashToken(token)},
		func(rows db.Scanner) error {
			for rows.Next() {
				if err := rows.Scan(&info.TaskID, &info.Hash, &expiresAt); err != nil {
					return err
				}
				found = true
			}
			return nil
		},
	)
	if err != nil {
		return TokenInfo{}, 0, fmt.Errorf("approval token: lookup: %w", err)
	}
	if !found {
		return TokenInfo{}, 0, ErrTokenInvalid
	}
	return info, expiresAt, nil
}

// hashToken returns the hex SHA-256 of a raw token — the storage key.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

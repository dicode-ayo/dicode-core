package relay

import (
	"context"
	"fmt"

	"github.com/dicode/dicode/pkg/db"
)

// kvKeyBrokerPubkey is the SQLite KV key where the daemon stores the
// broker's SPKI public key after trust-on-first-use discovery. The value
// is the raw base64 SPKI DER string received in the WSS welcome message.
const kvKeyBrokerPubkey = "relay.broker_pubkey"

// BrokerPubkeyPinResult describes the outcome of a TOFU pin check.
type BrokerPubkeyPinResult int

const (
	// BrokerPubkeyPinNew means no key was previously stored — first connect.
	BrokerPubkeyPinNew BrokerPubkeyPinResult = iota
	// BrokerPubkeyPinMatch means the received key matches the stored one.
	BrokerPubkeyPinMatch
	// BrokerPubkeyPinMismatch means the received key differs from the stored one.
	BrokerPubkeyPinMismatch
)

// CheckAndPinBrokerPubkey implements TOFU (trust-on-first-use) for the
// broker's signing public key.
//
//   - First connect (no stored key): stores the received key, returns PinNew.
//   - Subsequent connect (key matches): returns PinMatch. No DB write.
//   - Key changed: returns PinMismatch. Does NOT overwrite — the operator
//     must run `dicode relay trust-broker --yes` to accept the new key.
func CheckAndPinBrokerPubkey(ctx context.Context, database db.DB, received string) (BrokerPubkeyPinResult, error) {
	if received == "" {
		return BrokerPubkeyPinNew, nil // broker didn't announce a key — legacy mode
	}

	var stored string
	err := database.Query(ctx,
		`SELECT value FROM kv WHERE key = ?`,
		[]any{kvKeyBrokerPubkey},
		func(rows db.Scanner) error {
			if rows.Next() {
				return rows.Scan(&stored)
			}
			return nil
		},
	)
	if err != nil {
		return 0, fmt.Errorf("load broker pubkey: %w", err)
	}

	if stored == "" {
		// First time — trust on first use.
		if err := database.Exec(ctx,
			`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`,
			kvKeyBrokerPubkey, received,
		); err != nil {
			return 0, fmt.Errorf("store broker pubkey: %w", err)
		}
		return BrokerPubkeyPinNew, nil
	}

	if stored == received {
		return BrokerPubkeyPinMatch, nil
	}

	return BrokerPubkeyPinMismatch, nil
}

// ReplaceBrokerPubkey unconditionally overwrites the stored broker pubkey.
// Called by `dicode relay trust-broker --yes` when the operator explicitly
// accepts a new broker key.
func ReplaceBrokerPubkey(ctx context.Context, database db.DB, newKey string) error {
	return database.Exec(ctx,
		`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`,
		kvKeyBrokerPubkey, newKey,
	)
}

// LoadBrokerPubkey reads the currently pinned broker public key from the
// database. Returns "" if no key is pinned.
func LoadBrokerPubkey(ctx context.Context, database db.DB) (string, error) {
	var stored string
	err := database.Query(ctx,
		`SELECT value FROM kv WHERE key = ?`,
		[]any{kvKeyBrokerPubkey},
		func(rows db.Scanner) error {
			if rows.Next() {
				return rows.Scan(&stored)
			}
			return nil
		},
	)
	return stored, err
}

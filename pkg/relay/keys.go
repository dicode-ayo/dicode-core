package relay

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"

	"github.com/dicode/dicode/pkg/db"
	"go.uber.org/zap"
)

// kvKeyRelayPrivateKey is the legacy (pre-#104) KV row that stored the single
// dual-use ECDSA private key. After #104 the same row stores the SignKey only
// — the DecryptKey lives at kvKeyRelayDecryptPrivateKey. On upgrade the old row
// is reused as-is so the UUID (derived from the SignKey pubkey) stays stable
// and existing webhook URLs keep working.
const (
	kvKeyRelayPrivateKey        = "relay.private_key"
	kvKeyRelayDecryptPrivateKey = "relay.decrypt_private_key"
)

// Identity holds the relay keypairs and the derived stable UUID.
//
// Issue #104 split the single dual-use relay key into two keys with disjoint
// roles:
//
//   - SignKey    — ECDSA P-256, used for WSS handshake signatures and for
//     /auth/:provider query-string signatures. Never participates in ECDH.
//   - DecryptKey — ECDH P-256, used as the ECIES recipient key when the
//     broker delivers an encrypted OAuth token envelope.
//
// UUID remains derived from the SignKey uncompressed pubkey (sha256, hex) so
// daemons upgrading to #104 keep the same UUID and their shared webhook URLs
// (/u/<uuid>/hooks/...) do not break.
type Identity struct {
	SignKey    *ecdsa.PrivateKey // ECDSA P-256: WSS handshake + /auth/:provider sig
	DecryptKey *ecdsa.PrivateKey // ECDH P-256: OAuth token delivery (ECIES recipient)
	UUID       string            // hex(sha256(uncompressed SignKey pubkey)) — 64 chars
}

// SignPublicKey returns the 65-byte uncompressed P-256 public key for the
// SignKey. This is what the daemon advertises as `pubkey` in the WSS hello
// message and what the broker uses to verify ECDSA signatures.
func (id *Identity) SignPublicKey() []byte {
	return marshalUncompressed(&id.SignKey.PublicKey)
}

// DecryptPublicKey returns the 65-byte uncompressed P-256 public key for the
// DecryptKey. This is what the daemon advertises as `decrypt_pubkey` in the
// WSS hello message and what the broker uses as the ECIES recipient key when
// encrypting OAuth token deliveries.
func (id *Identity) DecryptPublicKey() []byte {
	return marshalUncompressed(&id.DecryptKey.PublicKey)
}

// marshalUncompressed serializes a P-256 public key as a 65-byte uncompressed
// point (0x04 || X || Y) without relying on the deprecated elliptic.Marshal.
func marshalUncompressed(pub *ecdsa.PublicKey) []byte {
	b := make([]byte, 65)
	b[0] = 0x04
	pub.X.FillBytes(b[1:33])
	pub.Y.FillBytes(b[33:65])
	return b
}

// unmarshalUncompressed parses a 65-byte uncompressed P-256 point.
// Uses crypto/ecdh for on-curve validation (elliptic.IsOnCurve is deprecated).
func unmarshalUncompressed(b []byte) (*ecdsa.PublicKey, error) {
	if len(b) != 65 || b[0] != 0x04 {
		return nil, fmt.Errorf("invalid uncompressed public key (len=%d)", len(b))
	}
	// Validate via ecdh.P256().NewPublicKey which performs an on-curve check.
	if _, err := ecdh.P256().NewPublicKey(b); err != nil {
		return nil, fmt.Errorf("public key point is not on P-256 curve: %w", err)
	}
	x := new(big.Int).SetBytes(b[1:33])
	y := new(big.Int).SetBytes(b[33:65])
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil
}

// deriveUUID computes hex(sha256(uncompressed pubkey bytes)).
func deriveUUID(pub *ecdsa.PublicKey) string {
	raw := marshalUncompressed(pub)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// LoadOrGenerateIdentity loads the relay identity from the database.
//
// Migration behaviour (issue #104):
//
//   - If the legacy relay.private_key row is present it becomes the SignKey.
//     The UUID is derived from its pubkey — bytes-identical to the pre-#104
//     UUID, so webhook URLs stay stable.
//   - If relay.decrypt_private_key is present it is parsed as the DecryptKey.
//   - If relay.decrypt_private_key is missing the function generates a fresh
//     P-256 key, inserts it (plain INSERT; never OR REPLACE, so an existing
//     row is never silently overwritten) and logs at INFO.
//   - If both rows are missing (fresh daemon) both keys are generated and
//     stored in a single call.
func LoadOrGenerateIdentity(ctx context.Context, database db.DB, log *zap.Logger) (*Identity, error) {
	if log == nil {
		log = zap.NewNop()
	}

	signPEM, err := loadKVKey(ctx, database, kvKeyRelayPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("load relay sign key: %w", err)
	}

	var signKey *ecdsa.PrivateKey
	if signPEM != "" {
		signKey, err = parseECPrivateKeyPEM(signPEM)
		if err != nil {
			return nil, fmt.Errorf("parse relay sign key: %w", err)
		}
	} else {
		signKey, err = generateAndStoreSignKey(ctx, database)
		if err != nil {
			return nil, err
		}
	}

	decryptPEM, err := loadKVKey(ctx, database, kvKeyRelayDecryptPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("load relay decrypt key: %w", err)
	}

	var decryptKey *ecdsa.PrivateKey
	if decryptPEM != "" {
		decryptKey, err = parseECPrivateKeyPEM(decryptPEM)
		if err != nil {
			return nil, fmt.Errorf("parse relay decrypt key: %w", err)
		}
	} else {
		decryptKey, err = generateAndStoreDecryptKey(ctx, database)
		if err != nil {
			return nil, err
		}
		log.Info("relay: generated fresh decrypt key (issue #104 split)")
	}

	return &Identity{
		SignKey:    signKey,
		DecryptKey: decryptKey,
		UUID:       deriveUUID(&signKey.PublicKey),
	}, nil
}

// loadKVKey reads a single KV row. Returns "" (no error) if the row is absent.
func loadKVKey(ctx context.Context, database db.DB, key string) (string, error) {
	var pemData string
	err := database.Query(ctx,
		`SELECT value FROM kv WHERE key = ?`,
		[]any{key},
		func(rows db.Scanner) error {
			if rows.Next() {
				return rows.Scan(&pemData)
			}
			return nil
		},
	)
	return pemData, err
}

// parseECPrivateKeyPEM decodes a PEM-encoded "EC PRIVATE KEY" block.
func parseECPrivateKeyPEM(pemData string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("invalid PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse EC private key: %w", err)
	}
	return key, nil
}

// encodeECPrivateKeyPEM encodes a P-256 private key as an "EC PRIVATE KEY" PEM
// block — the format both pre- and post-#104 rows share.
func encodeECPrivateKeyPEM(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("marshal EC private key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: der,
	})), nil
}

// RotateIdentity generates a fresh pair of P-256 keypairs (SignKey +
// DecryptKey), overwrites BOTH stored identity rows in a single transaction,
// and returns the new Identity.
//
// Both keys are rotated atomically: if one leaks, the other must be assumed
// leaked too (same process memory, same SQLite file, same backup). Rotating
// only one would leave a half-compromised surface with no operator-visible
// signal. The tx guarantees either both rows are replaced or neither is —
// the Identity the daemon will load on next boot is never a mix of old/new.
//
// The old keys are unrecoverable after this call. The UUID changes (it is
// derived from the new SignKey pubkey), so any public webhook URL the
// operator previously shared under the old UUID becomes permanently invalid;
// downstream consumers must be re-wired to the new
// relay.dicode.app/u/<new-uuid> base. Live WSS connections held by an
// in-memory Identity from before the rotation are not affected in this
// process — the caller is responsible for tearing them down and
// re-connecting with the new identity (typically by restarting the daemon).
func RotateIdentity(ctx context.Context, database db.DB) (*Identity, error) {
	signKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate relay sign key: %w", err)
	}
	decryptKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate relay decrypt key: %w", err)
	}

	signPEM, err := encodeECPrivateKeyPEM(signKey)
	if err != nil {
		return nil, err
	}
	decryptPEM, err := encodeECPrivateKeyPEM(decryptKey)
	if err != nil {
		return nil, err
	}

	if err := database.Tx(ctx, func(tx db.DB) error {
		if err := tx.Exec(ctx,
			`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`,
			kvKeyRelayPrivateKey, signPEM,
		); err != nil {
			return fmt.Errorf("store relay sign key: %w", err)
		}
		if err := tx.Exec(ctx,
			`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`,
			kvKeyRelayDecryptPrivateKey, decryptPEM,
		); err != nil {
			return fmt.Errorf("store relay decrypt key: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &Identity{
		SignKey:    signKey,
		DecryptKey: decryptKey,
		UUID:       deriveUUID(&signKey.PublicKey),
	}, nil
}

func generateAndStoreSignKey(ctx context.Context, database db.DB) (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate relay sign key: %w", err)
	}
	pemData, err := encodeECPrivateKeyPEM(key)
	if err != nil {
		return nil, err
	}
	// INSERT OR REPLACE is safe here: we only reach this path when the row is
	// absent. The fresh-daemon case wants to write it; a concurrent writer
	// writing the same key would race but that's a pathological scenario.
	if err := database.Exec(ctx,
		`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`,
		kvKeyRelayPrivateKey, pemData,
	); err != nil {
		return nil, fmt.Errorf("store relay sign key: %w", err)
	}
	return key, nil
}

func generateAndStoreDecryptKey(ctx context.Context, database db.DB) (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate relay decrypt key: %w", err)
	}
	pemData, err := encodeECPrivateKeyPEM(key)
	if err != nil {
		return nil, err
	}
	// Plain INSERT (not OR REPLACE) so we never silently overwrite an existing
	// decrypt key — if one exists, we should have read it above.
	if err := database.Exec(ctx,
		`INSERT INTO kv (key, value) VALUES (?, ?)`,
		kvKeyRelayDecryptPrivateKey, pemData,
	); err != nil {
		return nil, fmt.Errorf("store relay decrypt key: %w", err)
	}
	return key, nil
}

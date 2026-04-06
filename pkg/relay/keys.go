package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"

	"github.com/dicode/dicode/pkg/db"
)

const kvKeyRelayPrivateKey = "relay.private_key"

// Identity holds the relay keypair and the derived stable UUID.
type Identity struct {
	PrivateKey *ecdsa.PrivateKey
	UUID       string // hex(sha256(uncompressed pubkey)) — 64 chars
}

// UncompressedPublicKey returns the 65-byte uncompressed P-256 public key.
func (id *Identity) UncompressedPublicKey() []byte {
	pub := &id.PrivateKey.PublicKey
	return elliptic.Marshal(elliptic.P256(), pub.X, pub.Y)
}

// deriveUUID computes hex(sha256(uncompressed pubkey bytes)).
func deriveUUID(pub *ecdsa.PublicKey) string {
	raw := elliptic.Marshal(elliptic.P256(), pub.X, pub.Y)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// LoadOrGenerateIdentity loads the relay identity from the database.
// If no key exists yet, a new P-256 keypair is generated and stored.
func LoadOrGenerateIdentity(ctx context.Context, database db.DB) (*Identity, error) {
	var pemData string
	err := database.Query(ctx,
		`SELECT value FROM kv WHERE key = ?`,
		[]any{kvKeyRelayPrivateKey},
		func(rows db.Scanner) error {
			if rows.Next() {
				return rows.Scan(&pemData)
			}
			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("load relay key: %w", err)
	}

	if pemData != "" {
		return parseIdentity(pemData)
	}

	return generateAndStore(ctx, database)
}

func parseIdentity(pemData string) (*Identity, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("relay key: invalid PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("relay key: parse EC private key: %w", err)
	}
	return &Identity{
		PrivateKey: key,
		UUID:       deriveUUID(&key.PublicKey),
	}, nil
}

func generateAndStore(ctx context.Context, database db.DB) (*Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate relay keypair: %w", err)
	}

	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal relay key: %w", err)
	}
	pemData := string(pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: der,
	}))

	if err := database.Exec(ctx,
		`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`,
		kvKeyRelayPrivateKey, pemData,
	); err != nil {
		return nil, fmt.Errorf("store relay key: %w", err)
	}

	return &Identity{
		PrivateKey: key,
		UUID:       deriveUUID(&key.PublicKey),
	}, nil
}

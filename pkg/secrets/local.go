package secrets

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// LocalProvider stores secrets encrypted in SQLite using ChaCha20-Poly1305.
// The encryption key is derived from a master key via Argon2id.
//
// Master key resolution order:
//  1. DICODE_MASTER_KEY env var (base64-encoded 32 bytes)
//  2. ~/.dicode/master.key file (auto-generated on first run, chmod 600)
type LocalProvider struct {
	key []byte // 32-byte derived encryption key
	db  localDB
}

// localDB is the storage backend (implemented with sqlite in pkg/secrets/localdb.go).
type localDB interface {
	GetEncrypted(key string) (ciphertext []byte, nonce []byte, found bool, err error)
	SetEncrypted(key string, ciphertext []byte, nonce []byte) error
	Delete(key string) error
	List() ([]string, error)
}

// NewLocalProvider initialises the local encrypted secret store.
// dataDir is the dicode data directory (e.g. ~/.dicode).
func NewLocalProvider(dataDir string, db localDB) (*LocalProvider, error) {
	masterKey, err := loadOrCreateMasterKey(dataDir)
	if err != nil {
		return nil, fmt.Errorf("load master key: %w", err)
	}

	// Derive a 32-byte encryption key from the master key via Argon2id.
	// Salt is fixed per installation (stored alongside master key).
	saltPath := filepath.Join(dataDir, "master.salt")
	salt, err := loadOrCreateSalt(saltPath)
	if err != nil {
		return nil, fmt.Errorf("load salt: %w", err)
	}

	derivedKey := argon2.IDKey(masterKey, salt, 1, 64*1024, 4, 32)

	return &LocalProvider{key: derivedKey, db: db}, nil
}

func (l *LocalProvider) Name() string { return "local" }

func (l *LocalProvider) Get(_ context.Context, key string) (string, error) {
	ct, nonce, found, err := l.db.GetEncrypted(key)
	if err != nil {
		return "", fmt.Errorf("local store get %q: %w", key, err)
	}
	if !found {
		return "", nil
	}
	plaintext, err := l.decrypt(ct, nonce)
	if err != nil {
		return "", fmt.Errorf("decrypt secret %q: %w", key, err)
	}
	return string(plaintext), nil
}

func (l *LocalProvider) Set(_ context.Context, key, value string) error {
	ct, nonce, err := l.encrypt([]byte(value))
	if err != nil {
		return fmt.Errorf("encrypt secret %q: %w", key, err)
	}
	return l.db.SetEncrypted(key, ct, nonce)
}

func (l *LocalProvider) Delete(_ context.Context, key string) error {
	return l.db.Delete(key)
}

func (l *LocalProvider) List(_ context.Context) ([]string, error) {
	return l.db.List()
}

func (l *LocalProvider) encrypt(plaintext []byte) (ciphertext, nonce []byte, err error) {
	aead, err := chacha20poly1305.NewX(l.key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	ciphertext = aead.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

func (l *LocalProvider) decrypt(ciphertext, nonce []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(l.key)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ciphertext, nil)
}

// loadOrCreateMasterKey returns the raw master key bytes.
// Checks DICODE_MASTER_KEY env var first, then reads/creates the keyfile.
func loadOrCreateMasterKey(dataDir string) ([]byte, error) {
	if enc := os.Getenv("DICODE_MASTER_KEY"); enc != "" {
		key, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return nil, fmt.Errorf("decode DICODE_MASTER_KEY: %w", err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("DICODE_MASTER_KEY must be 32 bytes (base64-encoded)")
		}
		return key, nil
	}

	keyPath := filepath.Join(dataDir, "master.key")
	if data, err := os.ReadFile(keyPath); err == nil {
		key, err := base64.StdEncoding.DecodeString(string(data))
		if err != nil {
			return nil, fmt.Errorf("decode master.key: %w", err)
		}
		return key, nil
	}

	// Generate a new key on first run.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if err := os.WriteFile(keyPath, []byte(encoded), 0600); err != nil {
		return nil, fmt.Errorf("write master.key: %w", err)
	}
	return key, nil
}

func loadOrCreateSalt(path string) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil {
		return base64.StdEncoding.DecodeString(string(data))
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(salt)
	return salt, os.WriteFile(path, []byte(encoded), 0600)
}

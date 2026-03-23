package secrets

import (
	"context"
	"fmt"

	"github.com/dicode/dicode/pkg/db"
)

// SQLiteSecretDB implements localDB using the shared SQLite DB.
type SQLiteSecretDB struct {
	db db.DB
}

// NewSQLiteSecretDB wraps a DB handle as a secret store backend.
func NewSQLiteSecretDB(d db.DB) *SQLiteSecretDB {
	return &SQLiteSecretDB{db: d}
}

func (s *SQLiteSecretDB) GetEncrypted(key string) (ciphertext []byte, nonce []byte, found bool, err error) {
	err = s.db.Query(context.Background(),
		`SELECT ciphertext, nonce FROM secrets WHERE key = ?`,
		[]any{key},
		func(rows db.Scanner) error {
			if rows.Next() {
				found = true
				return rows.Scan(&ciphertext, &nonce)
			}
			return nil
		},
	)
	if err != nil {
		return nil, nil, false, fmt.Errorf("secrets get %q: %w", key, err)
	}
	return ciphertext, nonce, found, nil
}

func (s *SQLiteSecretDB) SetEncrypted(key string, ciphertext []byte, nonce []byte) error {
	return s.db.Exec(context.Background(),
		`INSERT INTO secrets (key, ciphertext, nonce) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET ciphertext = excluded.ciphertext, nonce = excluded.nonce`,
		key, ciphertext, nonce,
	)
}

func (s *SQLiteSecretDB) Delete(key string) error {
	return s.db.Exec(context.Background(),
		`DELETE FROM secrets WHERE key = ?`, key,
	)
}

func (s *SQLiteSecretDB) List() ([]string, error) {
	var keys []string
	err := s.db.Query(context.Background(),
		`SELECT key FROM secrets ORDER BY key`,
		nil,
		func(rows db.Scanner) error {
			for rows.Next() {
				var k string
				if err := rows.Scan(&k); err != nil {
					return err
				}
				keys = append(keys, k)
			}
			return nil
		},
	)
	return keys, err
}

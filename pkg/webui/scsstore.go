package webui

import (
	"context"
	"time"

	"github.com/dicode/dicode/pkg/db"
)

// scsStore implements github.com/alexedwards/scs/v2.Store (and IterableStore)
// on top of the dicode db.DB interface, using modernc.org/sqlite — no CGO.
type scsStore struct {
	db db.DB
}

func newSCSStore(d db.DB) *scsStore { return &scsStore{db: d} }

// Find returns the session data for a token. If the token does not exist or
// has expired, found is false.
func (s *scsStore) Find(token string) ([]byte, bool, error) {
	ctx := context.Background()
	var data []byte
	var found bool
	err := s.db.Query(ctx,
		`SELECT data FROM scs_sessions WHERE token = ? AND expires_at > ?`,
		[]any{token, time.Now().Unix()},
		func(rows db.Scanner) error {
			if rows.Next() {
				found = true
				return rows.Scan(&data)
			}
			return nil
		},
	)
	if err != nil {
		return nil, false, err
	}
	return data, found, nil
}

// Commit upserts a session token with data and expiry.
func (s *scsStore) Commit(token string, b []byte, expiry time.Time) error {
	ctx := context.Background()
	return s.db.Exec(ctx,
		`INSERT INTO scs_sessions (token, data, expires_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(token) DO UPDATE SET data = excluded.data, expires_at = excluded.expires_at`,
		token, b, expiry.Unix(),
	)
}

// Delete removes a session by token. No-op if not found.
func (s *scsStore) Delete(token string) error {
	return s.db.Exec(context.Background(),
		`DELETE FROM scs_sessions WHERE token = ?`, token)
}

// All returns every active session — required by scs.IterableStore so that
// SessionManager.Iterate works. Used by revokeAllSessions.
func (s *scsStore) All() (map[string][]byte, error) {
	ctx := context.Background()
	result := make(map[string][]byte)
	err := s.db.Query(ctx,
		`SELECT token, data FROM scs_sessions WHERE expires_at > ?`,
		[]any{time.Now().Unix()},
		func(rows db.Scanner) error {
			for rows.Next() {
				var tok string
				var data []byte
				if err := rows.Scan(&tok, &data); err != nil {
					return err
				}
				result[tok] = data
			}
			return nil
		},
	)
	return result, err
}

// deleteAll removes every session row. Used when the passphrase changes and
// all sessions must be invalidated.
func (s *scsStore) deleteAll() error {
	return s.db.Exec(context.Background(), `DELETE FROM scs_sessions`)
}

// purgeExpired removes expired rows. Called periodically.
func (s *scsStore) purgeExpired() error {
	return s.db.Exec(context.Background(),
		`DELETE FROM scs_sessions WHERE expires_at <= ?`, time.Now().Unix())
}

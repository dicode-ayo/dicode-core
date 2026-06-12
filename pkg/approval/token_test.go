package approval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
)

func newTestTokenStore(t *testing.T) *TokenStore {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return NewTokenStore(d)
}

func TestTokenMintAndRedeem(t *testing.T) {
	ts := newTestTokenStore(t)
	ctx := context.Background()

	token, err := ts.Mint(ctx, "repo/deploy", "hash-1")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(token, "dcap_") {
		t.Fatalf("token missing prefix: %q", token)
	}

	info, err := ts.Peek(ctx, token)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if info.TaskID != "repo/deploy" || info.Hash != "hash-1" {
		t.Fatalf("Peek binding = %+v", info)
	}

	info, err = ts.Redeem(ctx, token)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if info.TaskID != "repo/deploy" || info.Hash != "hash-1" {
		t.Fatalf("Redeem binding = %+v", info)
	}
}

func TestTokenSingleUse(t *testing.T) {
	ts := newTestTokenStore(t)
	ctx := context.Background()

	token, err := ts.Mint(ctx, "repo/deploy", "hash-1")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := ts.Redeem(ctx, token); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}
	if _, err := ts.Redeem(ctx, token); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("second Redeem err = %v, want ErrTokenInvalid", err)
	}
	if _, err := ts.Peek(ctx, token); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Peek after redeem err = %v, want ErrTokenInvalid", err)
	}
}

func TestTokenUnknownAndMalformed(t *testing.T) {
	ts := newTestTokenStore(t)
	ctx := context.Background()

	for _, tok := range []string{"", "garbage", "dcap_doesnotexist"} {
		if _, err := ts.Redeem(ctx, tok); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("Redeem(%q) err = %v, want ErrTokenInvalid", tok, err)
		}
	}
}

func TestTokenExpiry(t *testing.T) {
	ts := newTestTokenStore(t)
	ctx := context.Background()

	token, err := ts.Mint(ctx, "repo/deploy", "hash-1")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Move the clock past the TTL.
	ts.now = func() time.Time { return time.Now().Add(TokenTTL + time.Minute) }

	if _, err := ts.Peek(ctx, token); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("Peek expired err = %v, want ErrTokenExpired", err)
	}
	if _, err := ts.Redeem(ctx, token); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("Redeem expired err = %v, want ErrTokenExpired", err)
	}
	// The failed redemption consumed the row.
	if _, err := ts.Redeem(ctx, token); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("re-Redeem err = %v, want ErrTokenInvalid", err)
	}
}

func TestTokenMintRequiresBinding(t *testing.T) {
	ts := newTestTokenStore(t)
	if _, err := ts.Mint(context.Background(), "", "h"); err == nil {
		t.Error("Mint with empty task id must fail")
	}
	if _, err := ts.Mint(context.Background(), "repo/deploy", ""); err == nil {
		t.Error("Mint with empty hash must fail")
	}
}

func TestTokensAreUniqueAndUnguessableShape(t *testing.T) {
	ts := newTestTokenStore(t)
	ctx := context.Background()
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		tok, err := ts.Mint(ctx, "repo/deploy", "hash-1")
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if seen[tok] {
			t.Fatal("duplicate token minted")
		}
		seen[tok] = true
		// 32 random bytes → 43 base64url chars after the prefix.
		if len(tok) != len("dcap_")+43 {
			t.Fatalf("unexpected token length %d for %q", len(tok), tok)
		}
	}
}

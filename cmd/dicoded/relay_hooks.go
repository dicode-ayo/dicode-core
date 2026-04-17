package main

import (
	"context"
	"net/http"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/relay"
)

// buildRelayHooks adapts the relay package to the ipc.ControlServer so that
// cli.status and cli.relay.login have an identity-aware backend without ipc
// importing pkg/relay.
func buildRelayHooks(defaultBaseURL string, id *relay.Identity, database db.DB) ipc.RelayHooks {
	httpClient := &http.Client{Timeout: 20 * time.Second}

	status := func(ctx context.Context) (ipc.RelayStatus, error) {
		s := ipc.RelayStatus{Enabled: true, UUID: id.UUID}
		cs, err := relay.LoadClaimStatus(ctx, database)
		if err != nil {
			return s, nil // best effort — never block status on kv errors
		}
		s.Linked = cs.Linked()
		s.GithubLogin = cs.GithubLogin
		s.ClaimedAt = cs.ClaimedAt
		return s, nil
	}

	login := func(ctx context.Context, claimToken, label, baseURLOverride string) (ipc.RelayLoginResult, error) {
		baseURL := defaultBaseURL
		if baseURLOverride != "" {
			baseURL = baseURLOverride
		}
		result, err := relay.Claim(ctx, httpClient, baseURL, id, claimToken, label, database)
		if err != nil {
			return ipc.RelayLoginResult{}, err
		}
		return ipc.RelayLoginResult{UUID: result.UUID, GithubLogin: result.GithubLogin}, nil
	}

	return ipc.RelayHooks{Status: status, Login: login}
}

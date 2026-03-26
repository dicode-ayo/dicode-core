package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// NtfyNotifier sends notifications via ntfy (https://ntfy.sh).
// ntfy is Apache 2.0 licensed and can be self-hosted.
type NtfyNotifier struct {
	baseURL string
	topic   string
	token   string // optional Bearer token for auth
	client  *http.Client
}

func NewNtfyNotifier(baseURL, topic, tokenEnv string) *NtfyNotifier {
	token := ""
	if tokenEnv != "" {
		token = os.Getenv(tokenEnv)
	}
	return &NtfyNotifier{
		baseURL: strings.TrimRight(baseURL, "/"),
		topic:   topic,
		token:   token,
		client:  &http.Client{},
	}
}

func (n *NtfyNotifier) Name() string { return "ntfy" }

func (n *NtfyNotifier) Send(ctx context.Context, msg Message) error {
	payload := map[string]any{
		"topic":   n.topic,
		"message": msg.Body,
	}
	if msg.Title != "" {
		payload["title"] = msg.Title
	}
	if msg.Priority != "" && msg.Priority != PriorityDefault {
		payload["priority"] = string(msg.Priority)
	}
	if len(msg.Tags) > 0 {
		payload["tags"] = msg.Tags
	}
	if len(msg.Actions) > 0 {
		actions := make([]map[string]string, len(msg.Actions))
		for i, a := range msg.Actions {
			actions[i] = map[string]string{
				"action": "http",
				"label":  a.Label,
				// callback URL — filled in by the caller when approval gates are implemented
			}
		}
		payload["actions"] = actions
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.baseURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy returned status %d", resp.StatusCode)
	}
	return nil
}

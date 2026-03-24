package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// client calls the dicode REST API.
type client struct {
	base string
	http *http.Client
}

func newClient(host string, port int) *client {
	return &client{
		base: fmt.Sprintf("http://%s:%d", host, port),
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// StreamServerLogs connects to /logs/stream and sends formatted log lines to ch.
// It reconnects automatically on disconnect. Stops when ctx is cancelled.
func (c *client) StreamServerLogs(ctx context.Context, ch chan<- string) {
	go func() {
		for {
			_ = c.readSSE(ctx, ch)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
}

func (c *client) readSSE(ctx context.Context, ch chan<- string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/logs/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	// No timeout for the streaming client.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		raw := strings.TrimPrefix(line, "data: ")
		formatted := formatZapLine(raw)
		select {
		case ch <- formatted:
		case <-ctx.Done():
			return nil
		default: // drop if channel full — never block the HTTP reader
		}
	}
	return sc.Err()
}

// zapLine is the minimal shape of a zap JSON log entry.
type zapLine struct {
	Level string  `json:"level"`
	Ts    float64 `json:"ts"`
	Msg   string  `json:"msg"`
}

func formatZapLine(raw string) string {
	var entry zapLine
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return raw // pass through if not JSON
	}
	t := time.Unix(int64(entry.Ts), 0).Format("15:04:05")
	return fmt.Sprintf("%s %-5s %s", t, strings.ToUpper(entry.Level), entry.Msg)
}

// taskItem is the JSON shape returned by GET /api/tasks.
type taskItem struct {
	ID          string      `json:"ID"`
	Name        string      `json:"Name"`
	Description string      `json:"Description"`
	Trigger     triggerJSON `json:"Trigger"`
}

type triggerJSON struct {
	Cron    string `json:"Cron"`
	Webhook string `json:"Webhook"`
	Manual  bool   `json:"Manual"`
}

// runItem is the JSON shape returned by GET /api/tasks/{id}/runs.
type runItem struct {
	ID         string     `json:"ID"`
	TaskID     string     `json:"TaskID"`
	Status     string     `json:"Status"`
	StartedAt  time.Time  `json:"StartedAt"`
	FinishedAt *time.Time `json:"FinishedAt"`
}

// logLine is a single structured log entry from GET /api/runs/{id}/logs.
type logLine struct {
	Ts      time.Time `json:"Ts"`
	Level   string    `json:"Level"`
	Message string    `json:"Message"`
}

func (c *client) listTasks(ctx context.Context) ([]taskItem, error) {
	var out []taskItem
	return out, c.get(ctx, "/api/tasks", &out)
}

func (c *client) listRuns(ctx context.Context, taskID string) ([]runItem, error) {
	var out []runItem
	return out, c.get(ctx, "/api/tasks/"+taskID+"/runs", &out)
}

func (c *client) getLogs(ctx context.Context, runID string) ([]logLine, error) {
	var out []logLine
	return out, c.get(ctx, "/api/runs/"+runID+"/logs", &out)
}

func (c *client) triggerRun(ctx context.Context, taskID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/tasks/"+taskID+"/run", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		RunID string `json:"runId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.RunID, nil
}

func (c *client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, path)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

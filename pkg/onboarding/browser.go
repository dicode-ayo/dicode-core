package onboarding

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"
)

//go:embed wizard_assets
var wizardFS embed.FS

// buildWizardHandler wires the static wizard assets and the /setup/* API
// routes. It returns the handler plus a channel that fires exactly once
// with the Result a user submits via POST /setup/apply.
func buildWizardHandler(home string) (http.Handler, <-chan Result) {
	resCh := make(chan Result, 1)
	mux := http.NewServeMux()

	sub, _ := fs.Sub(wizardFS, "wizard_assets")

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			// Serve static assets at their own paths (e.g. /setup/main.js).
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "index missing", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(b)
	})

	mux.HandleFunc("/setup/main.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		b, err := fs.ReadFile(sub, "main.js")
		if err != nil {
			http.Error(w, "missing", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(b)
	})

	mux.HandleFunc("/setup/presets", func(w http.ResponseWriter, r *http.Request) {
		type preset struct {
			Name      string `json:"name"`
			Label     string `json:"label"`
			Desc      string `json:"desc"`
			DefaultOn bool   `json:"default_on"`
		}
		out := make([]preset, 0, len(TaskSetPresets))
		for _, p := range TaskSetPresets {
			out = append(out, preset{p.Name, p.Label, p.Desc, p.DefaultOn})
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("/setup/defaults", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"local_tasks_dir": home + "/dicode-tasks",
			"data_dir":        home + "/.dicode",
			"port":            defaultPort,
		})
	})

	mux.HandleFunc("/setup/apply", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			TaskSets      map[string]bool `json:"tasksets"`
			LocalTasksDir string          `json:"local_tasks_dir"`
			DataDir       string          `json:"data_dir"`
			Port          int             `json:"port"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&payload); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		res := Result{
			TaskSetsEnabled: payload.TaskSets,
			LocalTasksDir:   payload.LocalTasksDir,
			DataDir:         payload.DataDir,
			Port:            payload.Port,
			Passphrase:      GeneratePassphrase(),
		}
		if res.DataDir == "" {
			res.DataDir = home + "/.dicode"
		}
		if res.Port == 0 {
			res.Port = defaultPort
		}

		select {
		case resCh <- res:
		default:
			http.Error(w, "setup already submitted", http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"passphrase": res.Passphrase})
	})

	return mux, resCh
}

// RunBrowser starts an ephemeral HTTP wizard on a random loopback port,
// points the user's browser at it, and blocks until the user submits the
// form or ctx is cancelled. Gives the client ~60s after submit to read
// the success page before shutting the listener down.
func RunBrowser(ctx context.Context, home string) (Result, error) {
	handler, resCh := buildWizardHandler(home)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Result{}, fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	url := "http://" + ln.Addr().String()
	fmt.Printf("Open your browser to %s\n", url)
	_ = openBrowser(url) // best effort

	shutdown := func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}

	select {
	case res := <-resCh:
		// Let the client render the passphrase page, then tear the server down.
		time.AfterFunc(60*time.Second, shutdown)
		return res, nil
	case <-ctx.Done():
		shutdown()
		return Result{}, ctx.Err()
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("serve: %w", err)
	}
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

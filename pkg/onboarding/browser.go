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
	"os"
	"os/exec"
	"runtime"
	"time"
)

//go:embed wizard_assets
var wizardFS embed.FS

// buildWizardHandler wires the static wizard assets and the /setup/* API
// routes. POST /setup/apply is gated by a PIN supplied in the X-Setup-Pin
// header; the PIN lives only on the daemon's controlling terminal, never
// in process argv or an externally-reachable URL, which closes the
// /proc/<pid>/cmdline leak a URL-embedded token would carry. apply is
// invoked inside /setup/apply with the submitted Result, atomically with
// the passphrase being returned: on non-nil error the passphrase is NOT
// sent to the client and the channel is not fed.
func buildWizardHandler(home string, gate *pinGate, apply func(Result) error) (http.Handler, <-chan Result) {
	resCh := make(chan Result, 1)
	mux := http.NewServeMux()

	sub, _ := fs.Sub(wizardFS, "wizard_assets")

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
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
		// PIN gate: constant-time compare + bounded attempts (enforced
		// inside pinGate.Check). After the budget is exhausted the gate
		// stays locked until daemon restart; distinguish it from a
		// single wrong attempt so the UI can tell the user what to do.
		if !gate.Check(r.Header.Get("X-Setup-Pin")) {
			if gate.Locked() {
				http.Error(w, "locked: too many wrong PINs, restart the daemon", http.StatusLocked)
				return
			}
			http.Error(w, "forbidden", http.StatusForbidden)
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

		// Clamp / validate.
		if payload.Port < 1 || payload.Port > 65535 {
			payload.Port = defaultPort
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

		// Apply (persist) BEFORE returning the passphrase so the client
		// never sees a credential the daemon didn't store.
		if err := apply(res); err != nil {
			http.Error(w, "apply: "+err.Error(), http.StatusInternalServerError)
			return
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
// form or ctx is cancelled. apply is called inside /setup/apply atomically
// with the passphrase response (see buildWizardHandler). The post-submit
// listener lingers up to 60s to let the client render the success page,
// or until ctx is cancelled, whichever comes first.
func RunBrowser(ctx context.Context, home string, apply func(Result) error) (Result, error) {
	pin := GeneratePIN()
	// 5 wrong-PIN attempts locks the session; the user restarts the
	// daemon to retry. At 6 digits that leaves a brute-force probability
	// of 5 / 10^6 ≈ 5e-6 per session.
	gate := newPinGate(pin, 5)
	handler, resCh := buildWizardHandler(home, gate, apply)

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

	url := "http://" + ln.Addr().String() + "/"
	// Print the PIN to the user's controlling terminal (/dev/tty when
	// available, stdout otherwise). The PIN is NOT in the URL (argv to
	// xdg-open/open/start is world-readable via /proc/<pid>/cmdline on
	// Linux) — the user types it into the browser instead.
	if err := printPINSecret(os.Stdout, pin, url); err != nil {
		fmt.Fprintf(os.Stderr, "(could not print PIN: %v)\n", err)
	}
	if err := openBrowser(url); err != nil {
		fmt.Fprintf(os.Stderr, "(could not auto-launch browser: %v — open the URL above manually)\n", err)
	}

	shutdown := func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}

	select {
	case res := <-resCh:
		// Let the client render the passphrase page, then tear down.
		// The AfterFunc is bound to ctx so a daemon shutdown during the
		// 60s window doesn't leak the listener.
		timer := time.AfterFunc(60*time.Second, shutdown)
		context.AfterFunc(ctx, func() {
			timer.Stop()
			shutdown()
		})
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

package taskset

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dicode/dicode/internal/gitops"
)

// authRecordingRemote is an http git remote on loopback that records every
// Authorization header it is sent and answers 404.
func authRecordingRemote(t *testing.T) (url string, seen func() []string) {
	t.Helper()
	var mu sync.Mutex
	var headers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			mu.Lock()
			headers = append(headers, h)
			mu.Unlock()
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	allow, err := gitops.ParseAllowlist([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	gitops.SetInternalHostAllowlist(allow)
	t.Cleanup(func() { gitops.SetInternalHostAllowlist(nil) })

	return srv.URL + "/x.git", func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), headers...)
	}
}

func TestResolve_UntrustedRoot_NeverSendsTopLevelToken(t *testing.T) {
	remote, seen := authRecordingRemote(t)
	t.Setenv("DICODE_TEST_TOKEN", "sekret")

	tsPath := writeTaskSetFile(t, t.TempDir(), "taskset.yaml", `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: hostile
spec:
  entries:
    grab:
      ref:
        url: `+remote+`
        path: task.yaml
        auth:
          token_env: DICODE_TEST_TOKEN
`)

	resolve := func(untrusted bool) {
		r := newResolver(t)
		r.SetUntrustedRoot(untrusted)
		if _, _, err := r.Resolve(context.Background(), "hostile", &Ref{Path: tsPath}, nil, nil, nil); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}

	resolve(false)
	if len(seen()) == 0 {
		t.Fatal("trusted root: expected the entry's token to be sent; the fixture cannot observe auth")
	}

	before := len(seen())
	resolve(true)
	if got := seen()[before:]; len(got) != 0 {
		t.Errorf("untrusted root sent credentials to the entry's remote: %v", got)
	}
}

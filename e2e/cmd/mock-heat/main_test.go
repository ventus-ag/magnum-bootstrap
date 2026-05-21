package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMockHeatRoutes(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc((&server{dir: dir}).route))
	defer srv.Close()

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// healthz
	if code, body := get("/healthz"); code != 200 || body != "ok" {
		t.Fatalf("healthz = %d %q", code, body)
	}

	// Unknown node -> empty deployment set so the agent keeps polling.
	if code, body := get("/md/self"); code != 200 || !strings.Contains(body, `"deployments":[]`) {
		t.Fatalf("empty metadata = %d %q", code, body)
	}

	// Harness writes a metadata file -> it is served verbatim.
	want := `{"deployments":[{"id":"self-create-1","group":"script"}]}`
	if err := os.WriteFile(filepath.Join(dir, "self.md.json"), []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, body := get("/md/self"); code != 200 || body != want {
		t.Fatalf("served metadata = %d %q, want %q", code, body, want)
	}

	// Agent POSTs a signal -> it lands in <id>.signal.json for the harness.
	sig := `{"reconcile_status":"success","deploy_status_code":0}`
	resp, err := http.Post(srv.URL+"/signal/self-create-1", "application/json", strings.NewReader(sig))
	if err != nil {
		t.Fatalf("POST signal: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("signal status = %d", resp.StatusCode)
	}
	got, err := os.ReadFile(filepath.Join(dir, "self-create-1.signal.json"))
	if err != nil {
		t.Fatalf("read signal file: %v", err)
	}
	if string(got) != sig {
		t.Fatalf("signal file = %q, want %q", got, sig)
	}

	// Path traversal in the id/node segment is rejected.
	if code, _ := get("/md/..%2f..%2fetc"); code == 200 {
		t.Fatalf("expected traversal rejection, got 200")
	}
}

package host

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseSystemctlUnitFileState(t *testing.T) {
	if got := parseSystemctlUnitFileState("masked\n"); got != "masked" {
		t.Fatalf("expected masked, got %q", got)
	}
	if got := parseSystemctlUnitFileState(" enabled-runtime \n"); got != "enabled-runtime" {
		t.Fatalf("expected enabled-runtime, got %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestDownloadFileWithRetry(t *testing.T) {
	var attempts atomic.Int32
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if attempts.Add(1); attempts.Load() < 3 {
				return &http.Response{
					StatusCode: http.StatusBadGateway,
					Status:     "502 Bad Gateway",
					Body:       io.NopCloser(strings.NewReader("retry")),
					Header:     make(http.Header),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader("hello world")),
				Header:     make(http.Header),
			}, nil
		}),
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "file.txt")

	executor := NewExecutor(true, nil)
	executor.HTTPClient = client
	result, err := executor.DownloadFileWithRetry(context.Background(), "https://example.invalid/file.txt", target, 0o644, 5)
	if err != nil {
		t.Fatalf("DownloadFileWithRetry returned error: %v", err)
	}
	if !result.Changed {
		t.Fatalf("expected download to report changed")
	}
	if result.Change == nil {
		t.Fatalf("expected download to return change metadata")
	}
	if result.Change.Action != ActionCreate {
		t.Fatalf("expected create action, got %q", result.Change.Action)
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("failed to read target: %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("unexpected target content %q", string(data))
	}
}

func TestUpsertExportRemovesValue(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bashrc")
	if err := os.WriteFile(target, []byte("export no_proxy='example'\nexport OTHER='keep'\n"), 0o644); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	executor := NewExecutor(true, nil)
	change, err := executor.UpsertExport(target, "no_proxy", "", 0o644)
	if err != nil {
		t.Fatalf("UpsertExport returned error: %v", err)
	}
	if change == nil {
		t.Fatalf("expected change when removing export")
	}
	if change.Action != ActionUpdate {
		t.Fatalf("expected update action, got %q", change.Action)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("failed to read target: %v", err)
	}
	if string(data) != "export OTHER='keep'\n" {
		t.Fatalf("unexpected file content %q", string(data))
	}
}

func TestEnsureFileClassifiesReplace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}

	executor := NewExecutor(false, nil)
	change, err := executor.EnsureFile(target, []byte("new"), 0o644)
	if err != nil {
		t.Fatalf("EnsureFile returned error: %v", err)
	}
	if change == nil {
		t.Fatalf("expected change for content replacement")
	}
	if change.Action != ActionReplace {
		t.Fatalf("expected replace action, got %q", change.Action)
	}
}

func TestWaitForActiveStateRequiresStableWindow(t *testing.T) {
	states := []bool{false, true, false, true, true, true, true}
	index := 0

	got := waitForActiveState(func() bool {
		if index >= len(states) {
			return states[len(states)-1]
		}
		state := states[index]
		index++
		return state
	}, 200*time.Millisecond, 10*time.Millisecond, 30*time.Millisecond)

	if !got {
		t.Fatalf("expected service to eventually become stably active")
	}
	if index < 6 {
		t.Fatalf("expected repeated polls before success, got %d", index)
	}
}

func TestWaitForActiveStateTimesOutWhenServiceFlaps(t *testing.T) {
	state := false
	got := waitForActiveState(func() bool {
		state = !state
		return state
	}, 120*time.Millisecond, 10*time.Millisecond, 40*time.Millisecond)

	if got {
		t.Fatalf("expected unstable service to time out")
	}
}

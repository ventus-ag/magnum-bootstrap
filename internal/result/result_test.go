package result

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeSucceededFillsDeployStdout(t *testing.T) {
	r := Normalize(Result{Status: "succeeded", Step: "apply", Summary: "applied 31 phases"})
	if r.Reason != "applied 31 phases" {
		t.Errorf("Reason should default to Summary, got %q", r.Reason)
	}
	if r.DeployStatusCode != 0 {
		t.Errorf("succeeded DeployStatusCode = %d, want 0", r.DeployStatusCode)
	}
	if !strings.Contains(r.DeployStdout, "applied 31 phases") || !strings.Contains(r.DeployStdout, "step=apply") {
		t.Errorf("DeployStdout missing signal text: %q", r.DeployStdout)
	}
	if r.DeployStderr != "" {
		t.Errorf("succeeded should not set DeployStderr, got %q", r.DeployStderr)
	}
}

func TestNormalizeFailedSetsCodeAndStderr(t *testing.T) {
	r := Normalize(Result{
		Status:    "failed",
		Step:      "up",
		Summary:   "reconcile failed",
		ErrorCode: "PULUMI_UP",
		Warnings:  []string{"etcd slow"},
		Details:   map[string]string{"role": "master", "operation": "create"},
	})
	if r.DeployStatusCode != 1 {
		t.Errorf("failed DeployStatusCode = %d, want 1", r.DeployStatusCode)
	}
	if !strings.Contains(r.DeployStderr, "errorCode=PULUMI_UP") {
		t.Errorf("DeployStderr missing errorCode: %q", r.DeployStderr)
	}
	if !strings.Contains(r.DeployStderr, "warning=etcd slow") {
		t.Errorf("DeployStderr missing warning: %q", r.DeployStderr)
	}
	// Details are emitted sorted by key.
	if !strings.Contains(r.DeployStderr, "operation=create") || !strings.Contains(r.DeployStderr, "role=master") {
		t.Errorf("DeployStderr missing details: %q", r.DeployStderr)
	}
	if i, j := strings.Index(r.DeployStderr, "operation="), strings.Index(r.DeployStderr, "role="); i > j {
		t.Errorf("details should be sorted (operation before role): %q", r.DeployStderr)
	}
}

func TestWriteProducesHeatReadableJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reconciler-last-run.json")
	if err := Write(path, Result{Status: "succeeded", Step: "apply", Summary: "ok"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("result file should end with a newline")
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("result file is not valid JSON: %v", err)
	}
	if parsed["status"] != "succeeded" {
		t.Errorf("status = %v, want succeeded", parsed["status"])
	}
	// deploy_status_code is non-omitempty so Heat always finds it.
	if _, ok := parsed["deploy_status_code"]; !ok {
		t.Error("deploy_status_code must always be present for Heat")
	}
}

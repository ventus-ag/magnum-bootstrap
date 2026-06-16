package stopservices

import (
	"strings"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

// TestShouldDrainWorkerCordonOnly locks in that workers are never drained:
// their /etc/kubernetes/admin.conf authenticates as system:node:<node>, which
// the Node authorizer forbids from performing a cluster drain. The worker path
// must return false before touching the executor (nil here).
func TestShouldDrainWorkerCordonOnly(t *testing.T) {
	var cfg config.Config
	cfg.Shared.NodegroupRole = "worker"
	if shouldDrain(cfg, nil, "kubectl", "/etc/kubernetes/admin.conf") {
		t.Fatal("worker must be cordon-only (cannot perform a cluster drain)")
	}
}

func TestParseDrainBlockersSkipsDaemonSetsAndCompletedPods(t *testing.T) {
	input := strings.Join([]string{
		"default|api-123|Running||ReplicaSet|",
		"kube-system|node-exporter|Running||DaemonSet|",
		"default|finished-job|Succeeded||Job|",
		"default|terminating-api|Running|2026-04-14T12:00:00Z|ReplicaSet|kubernetes pvc-protection",
	}, "\n")

	got := parseDrainBlockers(input)
	if len(got) != 2 {
		t.Fatalf("parseDrainBlockers() returned %d blockers, want 2", len(got))
	}
	if got[0].Namespace != "default" || got[0].Name != "api-123" {
		t.Fatalf("unexpected first blocker: %+v", got[0])
	}
	if got[1].DeletionTimestamp != "2026-04-14T12:00:00Z" {
		t.Fatalf("expected terminating pod deletion timestamp, got %+v", got[1])
	}
	if got[1].Finalizers != "kubernetes pvc-protection" {
		t.Fatalf("expected finalizers to be preserved, got %+v", got[1])
	}
}

func TestSummarizeDrainBlockersIncludesTerminationContext(t *testing.T) {
	blockers := []drainBlocker{
		{
			Namespace:         "default",
			Name:              "terminating-api",
			OwnerKind:         "ReplicaSet",
			Phase:             "Running",
			DeletionTimestamp: "2026-04-14T12:00:00Z",
			Finalizers:        "kubernetes pvc-protection",
		},
	}

	summary := summarizeDrainBlockers(blockers)
	for _, want := range []string{
		"default/terminating-api",
		"owner=ReplicaSet",
		"phase=Running",
		"deleting=2026-04-14T12:00:00Z",
		"finalizers=kubernetes pvc-protection",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q does not contain %q", summary, want)
		}
	}
}

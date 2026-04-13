package stopservices

import (
	"strings"
	"testing"
)

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

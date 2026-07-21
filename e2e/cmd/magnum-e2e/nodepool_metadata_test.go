package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func npNode(labels map[string]string, taints []corev1.Taint) corev1.Node {
	if labels == nil {
		labels = map[string]string{}
	}
	labels["magnum.openstack.org/role"] = "worker"
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec:       corev1.NodeSpec{Taints: taints},
	}
}

// TestNodeMetadataMismatch checks the e2e verification logic that gates the
// nodepool-metadata op: present labels/taints must be detected, absent ones
// must be enforced, and a matching node returns "".
func TestNodeMetadataMismatch(t *testing.T) {
	present := npMetaStage{
		wantLabels: map[string]string{"e2e-team": "magnum"},
		wantTaints: []npExpectedTaint{{key: "e2e-dedicated", value: "np", effect: corev1.TaintEffectNoSchedule}},
	}
	absent := npMetaStage{
		absentLabels: []string{"e2e-team"},
		absentTaints: []string{"e2e-dedicated"},
	}

	tainted := []corev1.Taint{{Key: "e2e-dedicated", Value: "np", Effect: corev1.TaintEffectNoSchedule}}

	// Matches the present stage.
	if why := nodeMetadataMismatch(npNode(map[string]string{"e2e-team": "magnum"}, tainted), present); why != "" {
		t.Fatalf("expected match, got mismatch: %s", why)
	}
	// Missing label.
	if why := nodeMetadataMismatch(npNode(nil, tainted), present); why == "" {
		t.Fatal("expected mismatch for missing label")
	}
	// Wrong label value.
	if why := nodeMetadataMismatch(npNode(map[string]string{"e2e-team": "other"}, tainted), present); why == "" {
		t.Fatal("expected mismatch for wrong label value")
	}
	// Missing taint.
	if why := nodeMetadataMismatch(npNode(map[string]string{"e2e-team": "magnum"}, nil), present); why == "" {
		t.Fatal("expected mismatch for missing taint")
	}
	// Wrong magnum role (not a worker) is always a mismatch.
	badRole := npNode(map[string]string{"e2e-team": "magnum"}, tainted)
	badRole.Labels["magnum.openstack.org/role"] = "master"
	if why := nodeMetadataMismatch(badRole, present); why == "" {
		t.Fatal("expected mismatch when magnum role != worker")
	}

	// Absent stage: clean node matches.
	if why := nodeMetadataMismatch(npNode(nil, nil), absent); why != "" {
		t.Fatalf("expected clean node to match absent stage, got: %s", why)
	}
	// Absent stage: lingering label is a mismatch.
	if why := nodeMetadataMismatch(npNode(map[string]string{"e2e-team": "magnum"}, nil), absent); why == "" {
		t.Fatal("expected mismatch for lingering label")
	}
	// Absent stage: lingering taint is a mismatch.
	if why := nodeMetadataMismatch(npNode(nil, tainted), absent); why == "" {
		t.Fatal("expected mismatch for lingering taint")
	}
}

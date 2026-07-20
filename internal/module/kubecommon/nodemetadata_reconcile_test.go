package kubecommon

import (
	"sort"
	"strings"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

// recorder captures the kubectl commands reconcileNodeMetadata would run.
type recorder struct{ cmds []string }

func (r *recorder) run(args ...string) error {
	r.cmds = append(r.cmds, strings.Join(args, " "))
	return nil
}

func workerCfg(name string, labels map[string]string, taints []config.NodeTaint) config.Config {
	var cfg config.Config
	cfg.Shared.InstanceName = name
	cfg.Shared.NodegroupRole = "worker"
	cfg.Shared.NodegroupName = "np-0"
	cfg.Shared.KubeTag = "v1.31.4"
	cfg.Shared.NodeLabels = labels
	cfg.Shared.NodeTaints = taints
	cfg.Worker = &config.WorkerConfig{}
	return cfg
}

func nodeDoc(labels map[string]string, annotations map[string]string, taints []nodeTaintDoc) *nodeDocument {
	n := &nodeDocument{}
	n.Metadata.Labels = labels
	n.Metadata.Annotations = annotations
	n.Spec.Taints = taints
	return n
}

// hasCmd reports whether any recorded command contains all the given substrings.
func hasCmd(cmds []string, subs ...string) bool {
	for _, c := range cmds {
		ok := true
		for _, s := range subs {
			if !strings.Contains(c, s) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// Old cluster: worker already has the two magnum.openstack.org labels the
// kubelet self-set, no custom metadata, no managed annotations. The reconcile
// must be a pure no-op — no kubectl mutations at all (no node-role write, no
// annotation churn). This is the fleet-safety guarantee on binary rollout.
func TestReconcileOldClusterNoop(t *testing.T) {
	cfg := workerCfg("c1-minion-0", nil, nil)
	node := nodeDoc(
		map[string]string{
			"magnum.openstack.org/role":      "worker",
			"magnum.openstack.org/nodegroup": "np-0",
		},
		map[string]string{},
		nil,
	)
	var rec recorder
	changes, err := reconcileNodeMetadata(cfg, "c1-minion-0", node, rec.run, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.cmds) != 0 {
		t.Fatalf("old-cluster reconcile must issue no kubectl commands, got: %v", rec.cmds)
	}
	if len(changes) != 0 {
		t.Fatalf("old-cluster reconcile must report no changes, got: %v", changes)
	}
}

// Adding custom labels + a taint: the reconcile must set each, and record them
// in the managed-set annotations so they can later be removed.
func TestReconcileAddLabelsAndTaint(t *testing.T) {
	cfg := workerCfg("c1-minion-0",
		map[string]string{"team": "ml"},
		[]config.NodeTaint{{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}},
	)
	node := nodeDoc(
		map[string]string{
			"magnum.openstack.org/role":      "worker",
			"magnum.openstack.org/nodegroup": "np-0",
		},
		map[string]string{},
		nil,
	)
	var rec recorder
	if _, err := reconcileNodeMetadata(cfg, "c1-minion-0", node, rec.run, true); err != nil {
		t.Fatal(err)
	}
	if !hasCmd(rec.cmds, "label", "team=ml", "--overwrite") {
		t.Fatalf("expected label set, got: %v", rec.cmds)
	}
	if !hasCmd(rec.cmds, "taint", "dedicated=gpu:NoSchedule", "--overwrite") {
		t.Fatalf("expected taint set, got: %v", rec.cmds)
	}
	if !hasCmd(rec.cmds, "annotate", config.ManagedLabelsAnnotation, "team") {
		t.Fatalf("expected managed-labels annotation, got: %v", rec.cmds)
	}
	if !hasCmd(rec.cmds, "annotate", config.ManagedTaintsAnnotation, "dedicated:NoSchedule") {
		t.Fatalf("expected managed-taints annotation, got: %v", rec.cmds)
	}
}

// Removing: a label/taint that was previously managed but is no longer desired
// must be deleted; a foreign label + a foreign taint (never managed) must be
// left untouched.
func TestReconcileRemovesManagedPreservesForeign(t *testing.T) {
	// Desired: only team=ml remains; the previously-managed env=ci is dropped.
	cfg := workerCfg("c1-minion-0",
		map[string]string{"team": "ml"},
		nil, // all taints removed
	)
	node := nodeDoc(
		map[string]string{
			"magnum.openstack.org/role":      "worker",
			"magnum.openstack.org/nodegroup": "np-0",
			"team":                           "ml",
			"env":                            "ci",       // managed, now undesired
			"external.example.com/owner":     "platform", // foreign, must survive
		},
		map[string]string{
			config.ManagedLabelsAnnotation: "env,team",
			config.ManagedTaintsAnnotation: "dedicated:NoSchedule",
		},
		[]nodeTaintDoc{
			{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}, // managed, now undesired
			{Key: "node.kubernetes.io/unreachable", Effect: "NoExecute"}, // foreign condition taint
		},
	)
	var rec recorder
	if _, err := reconcileNodeMetadata(cfg, "c1-minion-0", node, rec.run, true); err != nil {
		t.Fatal(err)
	}
	if !hasCmd(rec.cmds, "label", "c1-minion-0", "env-") {
		t.Fatalf("expected managed label env removed, got: %v", rec.cmds)
	}
	if !hasCmd(rec.cmds, "taint", "dedicated:NoSchedule-") {
		t.Fatalf("expected managed taint removed, got: %v", rec.cmds)
	}
	for _, c := range rec.cmds {
		if strings.Contains(c, "external.example.com/owner") {
			t.Fatalf("foreign label must not be touched, got: %v", c)
		}
		if strings.Contains(c, "node.kubernetes.io/unreachable") {
			t.Fatalf("foreign condition taint must not be touched, got: %v", c)
		}
	}
}

// Preview mode (apply=false): the reconcile reports the planned changes but
// issues no kubectl mutations.
func TestReconcilePreviewNoMutations(t *testing.T) {
	cfg := workerCfg("c1-minion-0",
		map[string]string{"team": "ml"},
		[]config.NodeTaint{{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}},
	)
	node := nodeDoc(
		map[string]string{
			"magnum.openstack.org/role":      "worker",
			"magnum.openstack.org/nodegroup": "np-0",
		},
		map[string]string{},
		nil,
	)
	var rec recorder
	changes, err := reconcileNodeMetadata(cfg, "c1-minion-0", node, rec.run, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.cmds) != 0 {
		t.Fatalf("preview must not run kubectl, got: %v", rec.cmds)
	}
	if len(changes) == 0 {
		t.Fatal("preview must still report planned changes")
	}
}

// Second run with identical desired + already-converged node state = zero
// changes (idempotency).
func TestReconcileIdempotent(t *testing.T) {
	cfg := workerCfg("c1-minion-0",
		map[string]string{"team": "ml"},
		[]config.NodeTaint{{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}},
	)
	managedLabels := []string{"team"}
	sort.Strings(managedLabels)
	node := nodeDoc(
		map[string]string{
			"magnum.openstack.org/role":      "worker",
			"magnum.openstack.org/nodegroup": "np-0",
			"team":                           "ml",
		},
		map[string]string{
			config.ManagedLabelsAnnotation: "team",
			config.ManagedTaintsAnnotation: "dedicated:NoSchedule",
		},
		[]nodeTaintDoc{{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}},
	)
	var rec recorder
	changes, err := reconcileNodeMetadata(cfg, "c1-minion-0", node, rec.run, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.cmds) != 0 {
		t.Fatalf("converged reconcile must be a no-op, got: %v", rec.cmds)
	}
	if len(changes) != 0 {
		t.Fatalf("converged reconcile must report no changes, got: %v", changes)
	}
}

package config

import (
	"reflect"
	"testing"
)

func TestParseNodeLabels(t *testing.T) {
	labels, warnings := ParseNodeLabels("team=ml; env=prod;example.com/pool=gpu;;")
	want := map[string]string{"team": "ml", "env": "prod", "example.com/pool": "gpu"}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels = %v, want %v", labels, want)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
}

func TestParseNodeLabelsEmptyValue(t *testing.T) {
	labels, warnings := ParseNodeLabels("node-role.kubernetes.io/infra=")
	if labels["node-role.kubernetes.io/infra"] != "" {
		t.Fatalf("labels = %v", labels)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
}

func TestParseNodeLabelsInvalidAndReserved(t *testing.T) {
	labels, warnings := ParseNodeLabels("-bad=x;magnum.openstack.org/role=evil;ok=1;bad value=2;toolong" +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa=x")
	want := map[string]string{"ok": "1"}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels = %v, want %v", labels, want)
	}
	if len(warnings) != 4 {
		t.Fatalf("warnings = %v, want 4 entries", warnings)
	}
}

func TestParseNodeTaints(t *testing.T) {
	taints, warnings := ParseNodeTaints("dedicated=gpu:NoSchedule;maintenance:NoExecute; spot=:PreferNoSchedule")
	want := []NodeTaint{
		{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"},
		{Key: "maintenance", Value: "", Effect: "NoExecute"},
		{Key: "spot", Value: "", Effect: "PreferNoSchedule"},
	}
	if !reflect.DeepEqual(taints, want) {
		t.Fatalf("taints = %v, want %v", taints, want)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
}

func TestParseNodeTaintsRejects(t *testing.T) {
	taints, warnings := ParseNodeTaints(
		"dedicated=gpu:BadEffect;node-role.kubernetes.io/control-plane:NoSchedule;node.kubernetes.io/unreachable:NoExecute;dup=a:NoSchedule;dup=b:NoSchedule;novalue")
	want := []NodeTaint{{Key: "dup", Value: "a", Effect: "NoSchedule"}}
	if !reflect.DeepEqual(taints, want) {
		t.Fatalf("taints = %v, want %v", taints, want)
	}
	if len(warnings) != 5 {
		t.Fatalf("warnings = %v, want 5 entries", warnings)
	}
}

func TestNodeTaintString(t *testing.T) {
	if got := (NodeTaint{Key: "a", Value: "b", Effect: "NoSchedule"}).String(); got != "a=b:NoSchedule" {
		t.Fatalf("got %q", got)
	}
	if got := (NodeTaint{Key: "a", Effect: "NoExecute"}).String(); got != "a:NoExecute" {
		t.Fatalf("got %q", got)
	}
}

func TestKubeletSafeLabels(t *testing.T) {
	safe, apiOnly := KubeletSafeLabels(map[string]string{
		"team":                             "ml",
		"example.com/pool":                 "gpu",
		"node-role.kubernetes.io/infra":    "",
		"kubernetes.io/foo":                "x",
		"k8s.io/foo":                       "x",
		"node.kubernetes.io/instance-type": "big",
		"sub.kubelet.kubernetes.io/x":      "y",
	})
	wantSafe := map[string]string{
		"team":                             "ml",
		"example.com/pool":                 "gpu",
		"node.kubernetes.io/instance-type": "big",
		"sub.kubelet.kubernetes.io/x":      "y",
	}
	wantAPI := map[string]string{
		"node-role.kubernetes.io/infra": "",
		"kubernetes.io/foo":             "x",
		"k8s.io/foo":                    "x",
	}
	if !reflect.DeepEqual(safe, wantSafe) {
		t.Fatalf("safe = %v, want %v", safe, wantSafe)
	}
	if !reflect.DeepEqual(apiOnly, wantAPI) {
		t.Fatalf("apiOnly = %v, want %v", apiOnly, wantAPI)
	}
}

func TestParseNodeMetadataEmptyValues(t *testing.T) {
	// Old clusters after a stack update carry NODE_LABELS=""/NODE_TAINTS=""
	// (keys present, empty). Must behave exactly like absent keys.
	for _, raw := range []string{"", "   ", ";", " ; ; "} {
		labels, warnings := ParseNodeLabels(raw)
		if labels != nil || len(warnings) != 0 {
			t.Fatalf("ParseNodeLabels(%q) = %v, %v — want nil, none", raw, labels, warnings)
		}
		taints, warnings := ParseNodeTaints(raw)
		if taints != nil || len(warnings) != 0 {
			t.Fatalf("ParseNodeTaints(%q) = %v, %v — want nil, none", raw, taints, warnings)
		}
	}
}

// Old-cluster contract: heat-params WITHOUT the keys and heat-params WITH
// empty values must produce identical (empty) node metadata config — the new
// binary must be a strict no-op for this feature on both shapes.
func TestLoadNodeMetadataOldClusterShapes(t *testing.T) {
	base := "NODEGROUP_ROLE=worker\nKUBE_MASTER_IP=10.0.0.5\nINSTANCE_NAME=c1-minion-0\nKUBE_TAG=v1.31.4\n"
	for name, content := range map[string]string{
		"absent-keys": base,
		"empty-keys":  base + "NODE_LABELS=\"\"\nNODE_TAINTS=\"\"\n",
	} {
		dir := t.TempDir()
		path := dir + "/heat-params"
		if err := writeTestFile(path, content); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cfg.Shared.NodeLabels != nil {
			t.Fatalf("%s: NodeLabels = %v, want nil", name, cfg.Shared.NodeLabels)
		}
		if cfg.Shared.NodeTaints != nil {
			t.Fatalf("%s: NodeTaints = %v, want nil", name, cfg.Shared.NodeTaints)
		}
		if len(cfg.Shared.NodeMetadataWarnings) != 0 {
			t.Fatalf("%s: unexpected warnings %v", name, cfg.Shared.NodeMetadataWarnings)
		}
	}
}

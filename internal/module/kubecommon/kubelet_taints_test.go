package kubecommon

import (
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

// The master taint block must render byte-identical to the previous
// hardcoded string — any drift rewrites kubelet-config.yaml fleet-wide and
// triggers a spurious kubelet restart on upgrade.
func TestRenderRegisterWithTaintsMasterCompat(t *testing.T) {
	got := RenderRegisterWithTaints([]config.NodeTaint{MasterBuiltinTaint("v1.28.4")}, nil)
	want := `registerWithTaints:
  - effect: "NoSchedule"
    key: "node-role.kubernetes.io/control-plane"
`
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}

	got = RenderRegisterWithTaints([]config.NodeTaint{MasterBuiltinTaint("v1.23.17")}, nil)
	want = `registerWithTaints:
  - effect: "NoSchedule"
    key: "node-role.kubernetes.io/master"
`
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestRenderRegisterWithTaintsWorker(t *testing.T) {
	if got := RenderRegisterWithTaints(nil, nil); got != "" {
		t.Fatalf("untainted worker must render empty, got %q", got)
	}
	got := RenderRegisterWithTaints(nil, []config.NodeTaint{
		{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"},
		{Key: "maintenance", Effect: "NoExecute"},
	})
	want := `registerWithTaints:
  - effect: "NoSchedule"
    key: "dedicated"
    value: "gpu"
  - effect: "NoExecute"
    key: "maintenance"
`
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

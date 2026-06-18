package scenario

import (
	"fmt"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

// supportedMinors is the Kubernetes minor range the modules carry version maps
// for (CLAUDE.md: "from 1.20 till 1.36"). The reconciler must parse a faithful
// heat-params for every one of these and detect role/operation correctly — this
// is the cheap (no-VM) guard that complements the FCoS-VM tier, which only boots
// one or two versions per run.
var supportedMinors = func() []int {
	var m []int
	for minor := 20; minor <= 36; minor++ {
		m = append(m, minor)
	}
	return m
}()

func tagFor(minor int) string { return fmt.Sprintf("v1.%d.0", minor) }

// TestVersionMatrixRoleAndOperation round-trips a master and worker create for
// every supported minor through the production config loader and asserts role +
// operation detection hold across the whole range.
func TestVersionMatrixRoleAndOperation(t *testing.T) {
	for _, minor := range supportedMinors {
		tag := tagFor(minor)
		t.Run(tag, func(t *testing.T) {
			m := base(RoleMaster, OpCreate)
			m.KubeTag = tag
			mc := loadVia(t, m)
			if mc.Role() != config.RoleMaster {
				t.Errorf("%s master: Role() = %q, want master", tag, mc.Role())
			}
			if mc.Operation() != config.OperationCreate {
				t.Errorf("%s master: Operation() = %q, want create", tag, mc.Operation())
			}
			if mc.Shared.KubeTag != tag {
				t.Errorf("%s master: KubeTag = %q, want %q", tag, mc.Shared.KubeTag, tag)
			}
			if !mc.IsFirstMaster() {
				t.Errorf("%s: master-0 should be first master", tag)
			}

			w := base(RoleWorker, OpCreate)
			w.KubeTag = tag
			wc := loadVia(t, w)
			if wc.Role() != config.RoleWorker {
				t.Errorf("%s worker: Role() = %q, want worker", tag, wc.Role())
			}
			if wc.Operation() != config.OperationCreate {
				t.Errorf("%s worker: Operation() = %q, want create", tag, wc.Operation())
			}
		})
	}
}

// TestVersionMatrixOperationsAcrossVersions confirms upgrade and ca-rotate are
// detected at both ends of the supported range, not just the default v1.30.
// Upgrade is no longer a distinct operation (state-driven via KUBE_TAG delta),
// so an "upgrade" scenario resolves to create; only CA rotation is distinct.
func TestVersionMatrixOperationsAcrossVersions(t *testing.T) {
	for _, minor := range []int{20, 36} {
		tag := tagFor(minor)
		up := base(RoleMaster, OpUpgrade)
		up.KubeTag = tag
		if got := loadVia(t, up).Operation(); got != config.OperationCreate {
			t.Errorf("%s upgrade: Operation() = %q, want create (upgrade is not a distinct op)", tag, got)
		}
		rot := base(RoleMaster, OpCARotate)
		rot.KubeTag = tag
		if got := loadVia(t, rot).Operation(); got != config.OperationCARotate {
			t.Errorf("%s ca-rotate: Operation() = %q, want ca-rotate", tag, got)
		}
	}
}

// TestVersionMatrixLeadNodeRole pins the production rename: the control-plane
// node-role label changed from "master" to "control-plane" in Kubernetes 1.25
// (write-heat-params-master.sh / leadNodeRole). LEAD_NODE_ROLE_NAME must track
// that across the whole range.
func TestVersionMatrixLeadNodeRole(t *testing.T) {
	for _, minor := range supportedMinors {
		tag := tagFor(minor)
		m := base(RoleMaster, OpCreate)
		m.KubeTag = tag
		got := loadVia(t, m).Raw["LEAD_NODE_ROLE_NAME"]
		want := "control-plane"
		if minor < 25 {
			want = "master"
		}
		if got != want {
			t.Errorf("%s: LEAD_NODE_ROLE_NAME = %q, want %q", tag, got, want)
		}
	}
}

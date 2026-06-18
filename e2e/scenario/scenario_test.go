package scenario

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

func base(role Role, op Operation) Config {
	return Config{
		ClusterName:  "e2e",
		Role:         role,
		NodeIndex:    0,
		Operation:    op,
		NodeIP:       "10.0.0.10",
		MasterIP:     "10.0.0.10",
		KubeTag:      "v1.30.5",
		AuthURL:      "http://127.0.0.1:9511/v3",
		MagnumURL:    "http://127.0.0.1:9511/v1",
		ClusterUUID:  "11111111-1111-1111-1111-111111111111",
		CAKey:        "ca-key-pem",
		SAKey:        "sa-key-pem",
		SAPrivateKey: "sa-priv-pem",
		CARotationID: "rot-1",
	}
}

// loadVia writes the rendered heat-params and parses it through the real
// production loader, so the test exercises the exact role/operation detection
// the reconciler uses.
func loadVia(t *testing.T, c Config) config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "heat-params")
	if err := os.WriteFile(path, []byte(c.HeatParams()), 0o600); err != nil {
		t.Fatalf("write heat-params: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func TestScenariosTriggerIntendedOperation(t *testing.T) {
	cases := []struct {
		name    string
		role    Role
		op      Operation
		wantOp  config.Operation
		wantRol config.Role
	}{
		// Upgrade and resize are NOT distinct reconciler operations anymore — both
		// are ordinary state convergence, so Operation() reports create. Only an
		// active CA rotation is a distinct operation.
		{"master-create", RoleMaster, OpCreate, config.OperationCreate, config.RoleMaster},
		{"master-upgrade", RoleMaster, OpUpgrade, config.OperationCreate, config.RoleMaster},
		{"master-resize", RoleMaster, OpResize, config.OperationCreate, config.RoleMaster},
		{"master-ca-rotate", RoleMaster, OpCARotate, config.OperationCARotate, config.RoleMaster},
		{"worker-create", RoleWorker, OpCreate, config.OperationCreate, config.RoleWorker},
		{"worker-upgrade", RoleWorker, OpUpgrade, config.OperationCreate, config.RoleWorker},
		{"worker-resize", RoleWorker, OpResize, config.OperationCreate, config.RoleWorker},
		{"worker-ca-rotate", RoleWorker, OpCARotate, config.OperationCARotate, config.RoleWorker},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadVia(t, base(tc.role, tc.op))
			if got := cfg.Role(); got != tc.wantRol {
				t.Errorf("Role() = %q, want %q", got, tc.wantRol)
			}
			if got := cfg.Operation(); got != tc.wantOp {
				t.Errorf("Operation() = %q, want %q", got, tc.wantOp)
			}
		})
	}
}

// TestCARotateFallsBackWhenAlreadyApplied mirrors the periodic-run safety: once
// a rotation ID has been applied, the operation must fall back to create so the
// full ca-rotate phases don't re-run on every timer tick.
func TestCARotateFallsBackWhenAlreadyApplied(t *testing.T) {
	cfg := loadVia(t, base(RoleMaster, OpCARotate))
	if cfg.Operation() != config.OperationCARotate {
		t.Fatalf("expected ca-rotate before applied-id is set")
	}
	cfg.Trigger.AppliedCARotationID = cfg.Trigger.CARotationID
	if got := cfg.Operation(); got != config.OperationCreate {
		t.Errorf("after applying rotation id, Operation() = %q, want %q", got, config.OperationCreate)
	}
}

func TestFirstMasterDetection(t *testing.T) {
	cfg := loadVia(t, base(RoleMaster, OpCreate))
	if !cfg.IsFirstMaster() {
		t.Errorf("master-0 should be detected as first master (instance %q)", cfg.Shared.InstanceName)
	}
	worker := loadVia(t, base(RoleWorker, OpCreate))
	if worker.IsFirstMaster() {
		t.Errorf("worker should never be first master")
	}
}

// TestCloudProviderToggle verifies the single switch flips all OpenStack
// integration fields together, so the mock-VM tier never enables OCCM/CSI.
func TestCloudProviderToggle(t *testing.T) {
	off := loadVia(t, base(RoleMaster, OpCreate))
	if off.Shared.CloudProviderEnabled {
		t.Error("CloudProvider=false should leave CLOUD_PROVIDER_ENABLED off")
	}
	if off.Shared.CinderCSIEnabled || off.Shared.ManilaCSIEnabled || off.Shared.OctaviaEnabled {
		t.Error("CloudProvider=false must keep OCCM/Cinder/Manila/Octavia off")
	}

	c := base(RoleMaster, OpCreate)
	c.CloudProvider = true
	on := loadVia(t, c)
	if !on.Shared.CloudProviderEnabled || !on.Shared.CinderCSIEnabled || !on.Shared.ManilaCSIEnabled || !on.Shared.OctaviaEnabled {
		t.Error("CloudProvider=true must enable cloud-provider + Cinder + Manila + Octavia together")
	}
	if on.Shared.VolumeDriver != "cinder" {
		t.Errorf("CloudProvider=true should set VOLUME_DRIVER=cinder, got %q", on.Shared.VolumeDriver)
	}
}

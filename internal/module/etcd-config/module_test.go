package etcdconfig

import (
	"context"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

func TestRunSkipsActiveCARotation(t *testing.T) {
	cfg := config.Config{
		Shared: config.SharedConfig{
			KubeTag: "v1.24.0",
		},
		Master: &config.MasterConfig{},
		Trigger: config.TriggerConfig{
			CARotationID: "rotate-123",
		},
	}

	res, err := (Module{}).Run(context.Background(), cfg, moduleapi.Request{Apply: true})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("expected no changes during active CA rotation, got %d", len(res.Changes))
	}
	if got := res.Outputs["etcdTag"]; got != "3.5.6-0" {
		t.Fatalf("expected etcd tag output, got %q", got)
	}
}

func TestSkipMembershipReconcileIgnoresAlreadyAppliedCARotation(t *testing.T) {
	cfg := config.Config{
		Shared: config.SharedConfig{
			KubeTag: "v1.24.0",
		},
		Master: &config.MasterConfig{},
		Trigger: config.TriggerConfig{
			CARotationID:        "rotate-123",
			AppliedCARotationID: "rotate-123",
		},
	}

	if skipMembershipReconcile(cfg) {
		t.Fatalf("expected normal etcd membership reconciliation after applied CA rotation")
	}
}

func TestDiscoveryEndpointHealthRequired(t *testing.T) {
	tests := []struct {
		name            string
		numberOfMasters int
		want            bool
	}{
		{name: "unknown", numberOfMasters: 0, want: false},
		{name: "single master", numberOfMasters: 1, want: true},
		{name: "multi master", numberOfMasters: 3, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{
				Master: &config.MasterConfig{
					NumberOfMasters: tt.numberOfMasters,
				},
			}
			if got := discoveryEndpointHealthRequired(cfg); got != tt.want {
				t.Fatalf("expected %t, got %t", tt.want, got)
			}
		})
	}
}

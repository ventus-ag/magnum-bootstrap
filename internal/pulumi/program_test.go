package pulumi

import (
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/plan"
)

func TestBuildMetadataCarriesAppliedCARotationID(t *testing.T) {
	cfg := config.Config{Trigger: config.TriggerConfig{CARotationID: "rot-1", AppliedCARotationID: "rot-1"}}
	md := BuildMetadata(cfg, plan.Plan{}, "up", false, "/etc/sysconfig/heat-params")
	if md.AppliedCARotationID != "rot-1" {
		t.Fatalf("AppliedCARotationID = %q; the in-program reload would lose it and every module would see ca-rotate", md.AppliedCARotationID)
	}
	if md.Operation != config.OperationCreate.String() {
		t.Fatalf("operation = %q, want create", md.Operation)
	}
}

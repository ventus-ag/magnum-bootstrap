package heatcontaineragent

import (
	"testing"
	"time"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

func TestReadUptime(t *testing.T) {
	// /proc/uptime is the real thing here; we only assert it parses to a
	// plausible value, since unitActiveFor subtracts systemd's monotonic
	// timestamp from it and a bad parse would silently disable the heal.
	got, ok := readUptime()
	if !ok {
		t.Fatal("readUptime failed on a live /proc/uptime")
	}
	if got <= 0 {
		t.Errorf("uptime = %s, want > 0", got)
	}
}

// agentContainerSilent must answer "no wedge" for every case where it cannot
// prove one. The e2e periodic scenario runs on a node whose heat-container-agent
// unit is a no-op stub with NO container; an earlier marker-file-based check
// restarted that healthy agent on every periodic run and broke the steady-state
// idempotency invariant. podman is absent in the test environment, so this also
// covers the "runtime missing" fail-safe.
func TestAgentContainerSilentIsFailSafeWithoutAContainer(t *testing.T) {
	executor := host.NewExecutor(false, nil)
	if agentContainerSilent(executor) {
		t.Error("no agent container present, must not report a wedge")
	}
}

func TestAgentWedgeGraceIsGenerousEnoughForBoot(t *testing.T) {
	// A node that just booted has an active unit and no marker for a few
	// seconds. The grace must comfortably exceed that window, or a periodic
	// run landing right after boot would restart a perfectly healthy agent.
	if agentWedgeGrace < 5*time.Minute {
		t.Errorf("agentWedgeGrace = %s, too short to survive boot races", agentWedgeGrace)
	}
}

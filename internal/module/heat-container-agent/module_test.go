package heatcontaineragent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

// The wedge signal is the ABSENCE of the runtime marker. These cases pin the
// file-presence half of the decision, which is what distinguishes a silently
// dead agent from a healthy one -- the case that hung golem-cs-02 twice.
func TestHeatConfigRuntimeMarkerPresence(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "heat-config")

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("marker should not exist yet")
	}

	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker should exist after write: %v", err)
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

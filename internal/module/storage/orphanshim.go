package storage

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

// shimProc is one containerd-shim process observed on the host.
type shimProc struct {
	PID       int
	StartTick uint64 // starttime from /proc/<pid>/stat, in clock ticks since boot
}

// healOrphanShimWedge self-heals a node that was wedged by a PRE-FIX store
// relocation: the dedicated volume is already mounted (so the relocation path
// above never runs), but the old pods still run as orphan shims from the
// shadowed root-disk store — invisible to CRI, holding RWO volume mounts and
// host ports, so no replacement pod can start until they die.
//
// The heal is deliberately conservative — it only fires on the unambiguous
// wedge signature, and does nothing on any doubt:
//   - shim processes exist, AND
//   - the runtime knows ZERO containers AND ZERO sandboxes (a healthy node
//     with running shims always has entries), AND
//   - each victim shim started BEFORE the current runtime daemon (orphans by
//     definition predate the post-swap daemon; a pod being created right now
//     has a younger shim and is never touched).
//
// It kills only those orphans. The shadowed root-disk content and its lazy
// mount entries cannot be reclaimed while the volume is mounted — that space
// returns on the next reboot; the heal's job is to let pods start again.
func healOrphanShimWedge(executor *host.Executor, req moduleapi.Request, runtimeService string) []host.Change {
	if !req.Apply || runtimeService != "containerd" {
		return nil
	}
	shims := listShimProcesses(executor)
	if len(shims) == 0 {
		return nil
	}
	if n, err := runtimeKnownEntryCount(executor); err != nil || n > 0 {
		return nil // healthy (runtime knows its containers) or cannot tell — never touch
	}
	startTick, ok := runtimeMainStartTick(executor)
	if !ok {
		return nil
	}
	victims := orphanShims(shims, startTick)
	if len(victims) == 0 {
		return nil
	}
	if req.Logger != nil {
		req.Logger.Warnf("storage: wedge signature detected — %d orphan shim(s) from a shadowed pre-relocation store while the runtime knows zero containers; killing them so pods can start (shadowed root-disk space is reclaimed on next reboot)", len(victims))
	}
	var changes []host.Change
	for _, s := range victims {
		if err := executor.Run("kill", "-9", strconv.Itoa(s.PID)); err != nil {
			if req.Logger != nil {
				req.Logger.Warnf("storage: kill orphan shim pid %d: %v", s.PID, err)
			}
			continue
		}
		changes = append(changes, host.Change{Action: host.ActionDelete, Path: fmt.Sprintf("pid:%d", s.PID),
			Summary: "kill orphan containerd-shim from shadowed pre-relocation store"})
	}
	return changes
}

// orphanShims returns the shims that started before the current runtime
// daemon. Pure decision logic, unit-tested.
func orphanShims(shims []shimProc, runtimeStartTick uint64) []shimProc {
	var out []shimProc
	for _, s := range shims {
		if s.StartTick < runtimeStartTick {
			out = append(out, s)
		}
	}
	return out
}

// listShimProcesses finds running containerd-shim processes with their start
// ticks. Processes that vanish mid-scan are skipped.
func listShimProcesses(executor *host.Executor) []shimProc {
	out, err := executor.RunCapture("pgrep", "-f", "containerd-shim")
	if err != nil {
		return nil
	}
	var shims []shimProc
	for _, ln := range strings.Fields(out) {
		pid, err := strconv.Atoi(ln)
		if err != nil {
			continue
		}
		tick, ok := procStartTick(pid)
		if !ok {
			continue
		}
		shims = append(shims, shimProc{PID: pid, StartTick: tick})
	}
	return shims
}

// runtimeKnownEntryCount asks containerd how many containers + sandboxes it
// knows in the k8s.io namespace. An error means "cannot tell" — callers must
// treat that as unsafe to heal.
func runtimeKnownEntryCount(executor *host.Executor) (int, error) {
	ctr := "/srv/magnum/containerd-bundle/bin/ctr"
	if _, err := os.Stat(ctr); err != nil {
		ctr = "ctr"
	}
	out, err := executor.RunCapture(ctr, "--address", "/run/containerd/containerd.sock",
		"--namespace", "k8s.io", "containers", "ls", "-q")
	if err != nil {
		return 0, err
	}
	count := len(strings.Fields(out))
	// Sandboxes live in a separate store on containerd 2.x and may not appear
	// in `containers ls`. Best-effort: older ctr lacks the subcommand — a
	// failure there adds nothing (containers count already collected).
	if out, err := executor.RunCapture(ctr, "--address", "/run/containerd/containerd.sock",
		"--namespace", "k8s.io", "sandboxes", "ls", "-q"); err == nil {
		count += len(strings.Fields(out))
	}
	return count, nil
}

// runtimeMainStartTick returns the start tick of the containerd main process.
func runtimeMainStartTick(executor *host.Executor) (uint64, bool) {
	out, err := executor.RunCapture("systemctl", "show", "-p", "MainPID", "--value", "containerd")
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return procStartTick(pid)
}

// procStartTick reads the process start time (clock ticks since boot) from
// /proc/<pid>/stat.
func procStartTick(pid int) (uint64, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	return parseStatStartTick(string(data))
}

// parseStatStartTick extracts field 22 (starttime) from /proc/<pid>/stat
// content. The comm field (2) can contain spaces and parentheses, so fields
// are counted after the LAST ')'.
func parseStatStartTick(stat string) (uint64, bool) {
	idx := strings.LastIndexByte(stat, ')')
	if idx < 0 || idx+1 >= len(stat) {
		return 0, false
	}
	rest := strings.Fields(stat[idx+1:])
	// rest[0] is field 3 (state); starttime is field 22 → rest[19].
	if len(rest) < 20 {
		return 0, false
	}
	tick, err := strconv.ParseUint(rest[19], 10, 64)
	if err != nil {
		return 0, false
	}
	return tick, true
}

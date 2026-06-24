package config

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// hostMemTotalMiB returns total system RAM in MiB from /proc/meminfo, or 0 if it
// cannot be determined.
func hostMemTotalMiB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line) // "MemTotal:", "<kb>", "kB"
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}

// maxAutoParallelism is the historical default and the ceiling for auto-scaling.
const maxAutoParallelism = 10

// AutoParallelism picks a phase/Pulumi parallelism that converges fast on large
// nodes without OOM-killing small ones.
//
// A reconcile run drives the Pulumi engine plus its pulumi-kubernetes plugin
// subprocess, and each unit of parallelism is another concurrent Helm install
// held in that plugin's heap. On a 2 GiB single-master node — control plane +
// every cluster addon + the reconciler all on one host — a parallelism of 10
// grows the plugin past 700 MiB and the kernel OOM-kills it mid-run (pulumi
// dies → run-once exits 1 → Heat marks the stack UPDATE_FAILED and the node is
// left wedged). Scale the width to host RAM, then clamp by CPU count so tiny
// nodes serialize the work and large nodes stay fast.
func AutoParallelism() int {
	return parallelismFor(hostMemTotalMiB(), runtime.NumCPU())
}

// parallelismFor is the pure RAM/CPU -> parallelism mapping behind
// AutoParallelism, split out so it is unit-testable without touching the host.
func parallelismFor(memMiB, numCPU int) int {
	var p int
	switch {
	case memMiB <= 0:
		p = 4 // unknown RAM: conservative middle ground
	case memMiB < 2560: // ~2 GiB nodes (VC-2): serialize to survive
		p = 1
	case memMiB < 4096:
		p = 2
	case memMiB < 8192:
		p = 4
	case memMiB < 16384:
		p = 8
	default:
		p = maxAutoParallelism
	}

	if numCPU > 0 {
		if limit := numCPU * 2; p > limit {
			p = limit
		}
	}
	if p < 1 {
		p = 1
	}
	return p
}

package etcdconfig

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

// Day-2 maintenance thresholds.
const (
	// defragMinDBBytes skips defrag on small databases where the reclaim is
	// negligible and the brief stop-the-world is not worth it.
	defragMinDBBytes = 128 * 1024 * 1024 // 128 MiB
	// defragFragRatio triggers defrag once at least this fraction of the
	// allocated db file is free space (allocated by bbolt but not in use).
	defragFragRatio = 0.45
)

// etcdDay2Maintenance runs steady-state etcd housekeeping that must never happen
// during cluster convergence: alarm reconciliation and defragmentation. etcd's
// own auto-compaction (configured in etcd.conf.yaml) handles history compaction,
// but it does NOT reclaim the freed pages from the db file — only defrag does, so
// that is the real gap this fills.
//
// It is gated to: the periodic timer (never a Heat run-once), a non-upgrade
// window, a locally healthy started voter (this node), and a cluster whose other
// voters are currently healthy (so masters never defrag simultaneously and drop
// quorum). Entirely best-effort — every failure is logged and swallowed so a
// transient housekeeping error can never wedge an otherwise-healthy node.
func etcdDay2Maintenance(cfg config.Config, executor *host.Executor, nodeIP, protocol, certDir, lbEndpoint string, req moduleapi.Request) {
	if !req.Periodic || !executor.Apply {
		return
	}
	// A KUBE_TAG delta means a disruptive convergence (upgrade) is in flight on
	// the cluster — defer maintenance until it settles.
	if req.PreviousKubeTag != "" && req.PreviousKubeTag != cfg.Shared.KubeTag {
		return
	}

	localEndpoint := fmt.Sprintf("%s://%s:2379", protocol, nodeIP)
	if !etcdHealthy(executor, localEndpoint, certDir, cfg.Shared.TLSDisabled) {
		return
	}

	lbArgs := etcdctlArgs(lbEndpoint, certDir, cfg.Shared.TLSDisabled)
	myPeer := protocol + "://" + nodeIP + ":2380"

	// Confirm we are a started voter (not a learner mid-join) and capture the
	// member list for the other-voters-healthy quorum guard.
	out, err := runEtcdctl(executor, append(append([]string{}, lbArgs...), "member", "list")...)
	if err != nil {
		return
	}
	members := parseMemberList(out)
	self, ok := selectSelf(members, cfg.Shared.InstanceName, myPeer)
	if !ok || self.isLearner || !self.started {
		return
	}

	// 1. Alarm reconciliation (cheap, read-only; runs on every cluster size).
	nospace := reconcileEtcdAlarms(executor, lbArgs)

	// 2. Defragmentation (self only). HA-only: defrag briefly blocks the member
	// it runs on, so only do it where quorum absorbs one voter being momentarily
	// unavailable (>= 3 voters). On a single/2-master cluster a defrag would stall
	// the (only quorum-critical) apiserver backend, so skip it — alarms above
	// still ran.
	if votingMemberCount(members) < 3 {
		return
	}
	if !otherVotersHealthy(cfg, executor, members, myPeer, certDir) {
		if executor.Logger != nil {
			executor.Logger.Infof("etcd day-2: skipping defrag — another voter is not healthy (avoiding simultaneous defrag)")
		}
		return
	}
	defragged := maybeDefragLocal(executor, localEndpoint, certDir, cfg.Shared.TLSDisabled)

	// 3. Disarm NOSPACE only after a successful defrag has reclaimed space. etcd
	// re-arms it on the next over-quota write, so this is safe — never disarm
	// without first having freed space.
	if nospace && defragged {
		if _, derr := runEtcdctl(executor, append(append([]string{}, lbArgs...), "alarm", "disarm")...); derr != nil {
			if executor.Logger != nil {
				executor.Logger.Warnf("etcd day-2: alarm disarm after defrag failed: %v", derr)
			}
		} else if executor.Logger != nil {
			executor.Logger.Infof("etcd day-2: disarmed NOSPACE alarm after defrag reclaimed space")
		}
	}
}

// reconcileEtcdAlarms lists active etcd alarms and logs them. Returns true if a
// NOSPACE alarm is present. CORRUPT alarms are logged loudly but never
// auto-handled — they require operator intervention (restore from snapshot).
func reconcileEtcdAlarms(executor *host.Executor, lbArgs []string) bool {
	out, err := runEtcdctl(executor, append(append([]string{}, lbArgs...), "alarm", "list")...)
	if err != nil {
		return false
	}
	nospace, corrupt := parseAlarms(out)
	if executor.Logger != nil {
		if nospace {
			executor.Logger.Warnf("etcd day-2: NOSPACE alarm active (db over quota)")
		}
		if corrupt {
			executor.Logger.Warnf("etcd day-2: CORRUPT alarm active — manual recovery required (restore from snapshot)")
		}
	}
	return nospace
}

// parseAlarms scans `etcdctl alarm list` output for active alarms. Empty output
// (no alarms) yields false/false.
func parseAlarms(out string) (nospace, corrupt bool) {
	low := strings.ToLower(out)
	return strings.Contains(low, "nospace"), strings.Contains(low, "corrupt")
}

// epStatus is the subset of `etcdctl endpoint status -w json` we read.
type epStatus struct {
	dbSize      int64
	dbSizeInUse int64
}

// parseEndpointStatus reads dbSize/dbSizeInUse from `etcdctl endpoint status
// -w json` (a one-element array for a single endpoint). Tolerant of camelCase /
// snake_case key spellings across etcdctl versions.
func parseEndpointStatus(out string) (epStatus, bool) {
	// Status holds mixed value types (numbers, strings, bools), so decode each
	// field lazily as RawMessage and only number-parse the keys we need.
	var rows []struct {
		Status map[string]json.RawMessage `json:"Status"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) == 0 {
		return epStatus{}, false
	}
	s := rows[0].Status
	size, ok1 := firstNumber(s, "dbSize", "DbSize", "db_size")
	inUse, ok2 := firstNumber(s, "dbSizeInUse", "DbSizeInUse", "db_size_in_use")
	if !ok1 || !ok2 {
		return epStatus{}, false
	}
	return epStatus{dbSize: size, dbSizeInUse: inUse}, true
}

func firstNumber(m map[string]json.RawMessage, keys ...string) (int64, bool) {
	for _, k := range keys {
		if raw, ok := m[k]; ok {
			var n int64
			if err := json.Unmarshal(raw, &n); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// needsDefrag reports whether a db with the given allocated/in-use sizes is
// fragmented enough to be worth a stop-the-world defrag.
func needsDefrag(dbSize, dbSizeInUse, minBytes int64, ratio float64) bool {
	if dbSize < minBytes || dbSize <= 0 {
		return false
	}
	free := dbSize - dbSizeInUse
	if free <= 0 {
		return false
	}
	return float64(free)/float64(dbSize) >= ratio
}

// maybeDefragLocal defrags this node's own etcd (local endpoint only — never the
// LB, which would defrag an arbitrary member) when its db is fragmented past the
// threshold. Returns true only if a defrag actually ran and succeeded.
func maybeDefragLocal(executor *host.Executor, localEndpoint, certDir string, tlsDisabled bool) bool {
	statusArgs := etcdctlArgsWithTimeout(localEndpoint, certDir, tlsDisabled, "5s")
	out, err := runEtcdctl(executor, append(append([]string{}, statusArgs...), "endpoint", "status", "-w", "json")...)
	if err != nil {
		return false
	}
	st, ok := parseEndpointStatus(out)
	if !ok || !needsDefrag(st.dbSize, st.dbSizeInUse, defragMinDBBytes, defragFragRatio) {
		return false
	}
	if executor.Logger != nil {
		executor.Logger.Infof("etcd day-2: defragmenting local member (dbSize=%d dbSizeInUse=%d, %.0f%% free)",
			st.dbSize, st.dbSizeInUse, 100*float64(st.dbSize-st.dbSizeInUse)/float64(st.dbSize))
	}
	// Defrag can take a while on a large db; give it a generous timeout.
	defragArgs := etcdctlArgsWithTimeout(localEndpoint, certDir, tlsDisabled, "60s")
	if _, derr := runEtcdctl(executor, append(append([]string{}, defragArgs...), "defrag")...); derr != nil {
		if executor.Logger != nil {
			executor.Logger.Warnf("etcd day-2: defrag failed: %v", derr)
		}
		return false
	}
	if executor.Logger != nil {
		executor.Logger.Infof("etcd day-2: defrag complete")
	}
	return true
}

// votingMemberCount counts started voting members, excluding learners and
// not-yet-started members.
func votingMemberCount(members []etcdMember) int {
	n := 0
	for _, m := range members {
		if m.started && !m.isLearner {
			n++
		}
	}
	return n
}

// otherVotersHealthy reports whether every voting member OTHER than us currently
// answers a health probe on its client endpoint. Used to avoid defragging while
// another master is down or mid-defrag, which could momentarily drop quorum. A
// single-master cluster (no other voters) trivially returns true.
func otherVotersHealthy(cfg config.Config, executor *host.Executor, members []etcdMember, myPeer, certDir string) bool {
	for _, m := range members {
		if m.peerURL == myPeer || m.isLearner {
			continue
		}
		client := memberClientEndpoint(m.peerURL)
		if client == "" {
			continue
		}
		if !etcdHealthy(executor, client, certDir, cfg.Shared.TLSDisabled) {
			return false
		}
	}
	return true
}

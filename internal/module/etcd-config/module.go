package etcdconfig

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

// etcdImageTags maps K8s minor version to the etcd image tag that kubeadm bundles.
// Source: kubernetes/kubernetes cmd/kubeadm/app/constants/constants.go
var etcdImageTags = map[string]string{
	"1.35": "3.6.10-0",
}

func etcdTag(cfg config.Config) string {
	return config.LookupByKubeVersion(etcdImageTags, cfg.Shared.KubeTag)
}

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "etcd" }
func (Module) Dependencies() []string { return []string{"master-certificates"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if cfg.Master == nil {
		return moduleapi.Result{}, fmt.Errorf("etcd module requires master config")
	}

	// A pure CA rotation has already replaced certs and restarted etcd in the
	// ca-rotation module. Do not run LB membership/discovery logic here: while
	// masters are rotating, the LB can route to a peer still using the old CA,
	// which looks like an unhealthy cluster and can trigger destructive rejoin
	// decisions.
	if skipMembershipReconcile(cfg) {
		if req.Logger != nil {
			req.Logger.Infof("etcd: skipping membership reconciliation during active CA rotation rotationId=%s", cfg.Trigger.CARotationID)
		}
		return moduleapi.Result{
			Outputs: map[string]string{"etcdTag": etcdTag(cfg)},
		}, nil
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	nodeIP := cfg.ResolveNodeIP()
	protocol := "https"
	if cfg.Shared.TLSDisabled {
		protocol = "http"
	}
	certDir := "/etc/etcd/certs"

	// Volume preparation.
	cs, err := prepareVolume(cfg, executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, cs...)

	// Service file.
	cs, err = writeEtcdService(cfg, executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, cs...)

	// Install etcdctl.
	cs, err = installEtcdctl(cfg, executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, cs...)

	if !req.Apply {
		// Only report a planned change if etcd hasn't been configured yet.
		// On a converged node, preview should show zero etcd changes.
		_, configErr := os.Stat("/etc/etcd/etcd.conf.yaml")
		if configErr != nil {
			changes = append(changes, host.Change{
				Action:  host.ActionOther,
				Summary: "etcd cluster join/creation (planned)",
			})
		}
		return moduleapi.Result{Changes: changes}, nil
	}

	// Excess-member reconciliation (scale-down): master-0 evicts any etcd member
	// beyond the desired master count. This is purely state-driven (live member
	// count vs NumberOfMasters), idempotent, and a no-op once converged, so it
	// runs on every reconcile regardless of operation. It used to be gated behind
	// IS_RESIZE with an early return, which also wrongly skipped the join path —
	// so a newly added master (scale-up) never joined etcd. The reconciler decides
	// from cluster/node state, not from an operation flag.
	cleanupExcessMembers(cfg, executor, protocol, certDir)

	// Cluster join/creation logic.
	lbEndpoint := fmt.Sprintf("%s://%s:2379", protocol, cfg.Master.EtcdLBVIP)
	localEndpoint := fmt.Sprintf("%s://%s:2379", protocol, nodeIP)

	// INVARIANT A — data-dir gate. A populated WAL means this node has persisted
	// raft state and IS an existing cluster member. etcd ignores the
	// initial-cluster* fields on restart and recovers membership from the WAL, so
	// we must NEVER wipe the data dir or run a destructive rejoin here. This is the
	// single most important safety property: it covers process restart, node reboot
	// (etcd Cinder volume reattached), binary/etcd upgrade, periodic reconcile, and
	// quorum loss (etcd starts but stays leaderless until peers return).
	if walPresent() {
		if req.Logger != nil {
			req.Logger.Infof("etcd: persisted WAL present — starting with existing data, no wipe/rejoin")
		}
		lbOK := etcdHealthy(executor, lbEndpoint, certDir, cfg.Shared.TLSDisabled)
		cs, err := startWithExistingData(cfg, executor, nodeIP, protocol, certDir, lbEndpoint, lbOK)
		changes = append(changes, cs...)
		if err != nil {
			// Data present but etcd will not start. If the cluster is reachable and
			// we are no longer a member, the data is orphaned (node removed while
			// down) and unusable — wipe and rejoin. Otherwise fail safe: refuse to
			// destroy data on a transient or quorum-loss condition.
			if lbOK && !checkMembership(executor, lbEndpoint, certDir, cfg.Shared.TLSDisabled, cfg.Shared.InstanceName, nodeIP) {
				if req.Logger != nil {
					req.Logger.Warnf("etcd: have data but not a member and etcd will not start — orphaned data, wiping and rejoining: %v", err)
				}
				cleanupEtcd(executor)
				cs2, jerr := joinExistingCluster(cfg, executor, nodeIP, protocol, certDir, lbEndpoint)
				changes = append(changes, cs2...)
				if jerr != nil {
					return moduleapi.Result{}, jerr
				}
			} else {
				return moduleapi.Result{}, fmt.Errorf("etcd has persisted data but did not become active; cluster unreachable or still a member — refusing to destroy data (likely quorum loss, manual recovery required): %w", err)
			}
		}
		return moduleapi.Result{Changes: changes, Outputs: map[string]string{"etcdTag": etcdTag(cfg)}}, nil
	}

	// No local etcd data — fresh node: initial create, resize-added master,
	// auto-healed/recreated node, or disk-loss replacement. Always probe the LB
	// (an existing cluster may be running) before deciding.
	lbOK := etcdHealthy(executor, lbEndpoint, certDir, cfg.Shared.TLSDisabled)
	localOK := false
	isMember := false
	if _, configErr := os.Stat("/etc/etcd/etcd.conf.yaml"); configErr == nil {
		localOK = etcdHealthy(executor, localEndpoint, certDir, cfg.Shared.TLSDisabled)
	}
	if lbOK {
		isMember = checkMembership(executor, lbEndpoint, certDir, cfg.Shared.TLSDisabled, cfg.Shared.InstanceName, nodeIP)
	}
	if req.Logger != nil {
		req.Logger.Infof("etcd: hasData=false lbOK=%t localOK=%t isMember=%t lbEndpoint=%s localEndpoint=%s",
			lbOK, localOK, isMember, lbEndpoint, localEndpoint)
	}

	// Non-first master on a fresh multi-master create: master-0 forms the seed
	// etcd cluster and the other masters join through the LB. On a parallel Heat
	// create a non-first master can reach this phase before master-0's etcd is
	// routable, so a single LB probe sees lbOK=false. Wait for the seed before
	// deciding — bootstrapping ourselves would split-brain the cluster. The join
	// itself (memberAddWithRetry) tolerates the transient strict-reconfig-check
	// rejection that occurs while peers are still starting.
	if needsSeedWait(cfg, lbOK, localOK, isMember) {
		lbOK = waitForSeedEtcd(executor, lbEndpoint, certDir, cfg.Shared.TLSDisabled)
		if lbOK {
			isMember = checkMembership(executor, lbEndpoint, certDir, cfg.Shared.TLSDisabled, cfg.Shared.InstanceName, nodeIP)
		}
	}

	switch {
	case isMember && localOK:
		// Healthy member with config but no WAL (very unlikely past the gate).
		// Rebuild config only for TLS/proxy drift.
		if err := rebuildConfigIfNeeded(cfg, executor, nodeIP, protocol, certDir); err != nil {
			return moduleapi.Result{}, err
		}

	case isMember && !localOK:
		// Registered as a member (e.g. recreated node, same name, new IP) but no
		// local etcd and no data — rejoin: remove the stale entry and re-add.
		cs, err := rejoinCluster(cfg, executor, nodeIP, protocol, certDir, lbEndpoint)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)

	case lbOK:
		// Cluster exists and we are not a member — join via runtime reconfig.
		cleanupEtcd(executor)
		cs, err := joinExistingCluster(cfg, executor, nodeIP, protocol, certDir, lbEndpoint)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)

	case localOK:
		// Local etcd healthy but not an LB member and no WAL (single-node, LB
		// down). Rebuild config only for TLS/proxy drift; never re-bootstrap.
		if err := rebuildConfigIfNeeded(cfg, executor, nodeIP, protocol, certDir); err != nil {
			return moduleapi.Result{}, err
		}

	default:
		// No reachable cluster and no local data.
		// INVARIANT B — split-brain guard (state-driven): a multi-master node that
		// has previously converged (PreviousSuccessfulGeneration set) but now finds
		// no data and no reachable cluster has lost quorum or is temporarily
		// partitioned; fabricating a fresh cluster would split-brain it, so we
		// refuse. A genuinely fresh node (no prior successful generation) falls
		// through to bootstrap. Non-first masters are additionally protected by
		// staticInitialClusterMembers below, which never self-bootstraps.
		if req.PreviousSuccessfulGeneration != "" && cfg.Master.NumberOfMasters > 1 {
			return moduleapi.Result{}, fmt.Errorf("etcd: no reachable cluster and no local data on a multi-master node that previously converged — refusing to bootstrap a new cluster (split-brain guard); cluster likely lost quorum or is temporarily unreachable, manual recovery required")
		}
		// Bootstrap a new cluster from a static initial-cluster member list. A
		// first/single master uses its own peer URL (a one-node cluster that grows
		// via member-add as other masters join through the LB).
		initialCluster, ok := staticInitialClusterMembers(cfg, nodeIP, protocol)
		if !ok {
			return moduleapi.Result{}, fmt.Errorf("etcd: no healthy LB endpoint and cannot determine an initial-cluster (non-first master with no reachable cluster to join)")
		}
		cleanupEtcd(executor)
		etcdConf := buildConfig(cfg, nodeIP, protocol, "new-static", initialCluster)
		// A single-member list forms immediately, so wait for health; a
		// multi-member static cluster only reaches quorum once peers start.
		waitForEndpointHealth := !strings.Contains(initialCluster, ",")
		cs, err := writeAndStartEtcd(executor, etcdConf, protocol, nodeIP, certDir, cfg.Shared.TLSDisabled, waitForEndpointHealth)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"etcdTag": etcdTag(cfg)},
	}, nil
}

func prepareVolume(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	if cfg.Master.EtcdVolumeSize <= 0 {
		return nil, nil
	}

	// Already mounted — nothing to do.
	if executor.IsMountpoint("/var/lib/etcd") {
		return nil, nil
	}

	volume := cfg.Master.EtcdVolume
	if volume == "" {
		return nil, nil
	}
	devicePath := ""
	devicePath, err := findEtcdVolumeDevice(cfg, executor)
	if devicePath == "" {
		return nil, fmt.Errorf("etcd volume device for volume %q never appeared in /dev/disk/by-id after 60 attempts", volume)
	}

	var changes []host.Change

	// Format only if not already xfs.
	if executor.Apply {
		fstype, _ := executor.RunCapture("blkid", "-s", "TYPE", "-o", "value", devicePath)
		if strings.TrimSpace(fstype) != "xfs" {
			_ = executor.Run("mkfs.xfs", "-f", devicePath)
			changes = append(changes, host.Change{Action: host.ActionCreate, Path: devicePath, Summary: "format etcd volume as xfs"})
		}
	}

	dirResult, err := (hostresource.DirectorySpec{Path: "/var/lib/etcd", Mode: 0o755}).Apply(executor)
	if err != nil {
		return nil, err
	}
	changes = append(changes, dirResult.Changes...)

	fstabLine := fmt.Sprintf("%s /var/lib/etcd xfs defaults 0 0", devicePath)
	lineResult, err := (hostresource.LineSpec{Path: "/etc/fstab", Line: fstabLine, Mode: 0o644}).Apply(executor)
	if err != nil {
		return nil, err
	}
	if lineResult.Changed {
		changes = append(changes, lineResult.Changes...)
		_ = executor.Run("mount", "-a")
		_ = executor.Run("chown", "-R", "etcd.etcd", "/var/lib/etcd")
		_ = executor.Run("chmod", "755", "/var/lib/etcd")
	}

	return changes, nil
}

func findEtcdVolumeDevice(cfg config.Config, executor *host.Executor) (string, error) {
	if cfg.Master == nil || cfg.Master.EtcdVolume == "" {
		return "", nil
	}
	prefix := cfg.Master.EtcdVolume
	if len(prefix) > 20 {
		prefix = prefix[:20]
	}
	devicePath := ""
	for attempt := 0; attempt < 60; attempt++ {
		_ = executor.Run("udevadm", "trigger")
		entries, err := os.ReadDir("/dev/disk/by-id")
		if err == nil {
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), prefix) {
					devicePath = "/dev/disk/by-id/" + entry.Name()
					break
				}
			}
		}
		if devicePath != "" {
			return devicePath, nil
		}
		if executor.Apply {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return "", fmt.Errorf("etcd volume device with prefix %q never appeared in /dev/disk/by-id after 60 attempts", prefix)
}

func writeEtcdService(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	if !cfg.Shared.UsePodman {
		return nil, nil
	}

	content := buildEtcdService(cfg)
	fileResult, err := (hostresource.FileSpec{Path: "/etc/systemd/system/etcd.service", Content: []byte(content), Mode: 0o644}).Apply(executor)
	if err != nil {
		return nil, err
	}
	if fileResult.Changed {
		serviceResult, err := (hostresource.SystemdServiceSpec{Unit: "etcd", SkipIfMissing: true, DaemonReload: true}).Apply(executor)
		if err != nil {
			return nil, err
		}
		return append(fileResult.Changes, serviceResult.Changes...), nil
	}
	return nil, nil
}

func buildEtcdService(cfg config.Config) string {
	containerImage := cfg.Shared.ContainerInfraPrefix
	if containerImage == "" {
		containerImage = "registry.k8s.io/"
	}
	containerImage += "etcd"

	return fmt.Sprintf(`[Unit]
Description=Etcd server
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/sysconfig/heat-params
ExecStartPre=mkdir -p /var/lib/etcd
ExecStartPre=-/bin/podman rm etcd
ExecStart=/bin/podman run \
    --name etcd \
    --volume /etc/pki/ca-trust/extracted/pem:/etc/ssl/certs:ro,z \
    --volume /etc/etcd:/etc/etcd:ro,z \
    --volume /var/lib/etcd:/var/lib/etcd:rshared,z \
    --net=host \
    %s:%s \
    /usr/local/bin/etcd \
    --config-file /etc/etcd/etcd.conf.yaml
ExecStop=/bin/podman stop etcd
TimeoutStartSec=10min
IOSchedulingClass=best-effort
IOSchedulingPriority=0
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`, containerImage, etcdTag(cfg))
}

func installEtcdctl(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	// etcdTag returns kubeadm-style tags like "3.5.24-0"; strip the "-0"
	// image build suffix to get the upstream release version for downloads.
	etcdVersion := desiredEtcdctlVersion(cfg)
	if etcdVersion == "" {
		return nil, nil
	}
	if etcdctlVersionMatches(executor, etcdVersion) {
		return nil, nil
	}

	etcdDir := "/srv/magnum/etcd"
	tgzURL := fmt.Sprintf("https://github.com/etcd-io/etcd/releases/download/v%s/etcd-v%s-linux-amd64.tar.gz",
		etcdVersion, etcdVersion)
	tgzPath := fmt.Sprintf("%s/etcd-v%s-linux-amd64.tar.gz", etcdDir, etcdVersion)

	if !executor.Apply {
		action := host.ActionReplace
		if _, err := os.Stat("/usr/local/bin/etcdctl"); os.IsNotExist(err) {
			action = host.ActionCreate
		}
		return []host.Change{{
			Action:  action,
			Path:    "/usr/local/bin/etcdctl",
			Summary: fmt.Sprintf("install etcdctl %s from %s", etcdVersion, tgzURL),
		}}, nil
	}

	dirResult, err := (hostresource.DirectorySpec{Path: etcdDir, Mode: 0o755}).Apply(executor)
	if err != nil {
		return nil, err
	}
	var changes []host.Change
	changes = append(changes, dirResult.Changes...)

	dl, err := (hostresource.DownloadSpec{URL: tgzURL, Path: tgzPath, Mode: 0o644, Retries: 5}).ApplyWithResultContext(context.Background(), executor)
	if err != nil {
		return nil, fmt.Errorf("download etcdctl: %w", err)
	}
	if dl.Change != nil {
		changes = append(changes, *dl.Change)
	}

	if dl.Changed && executor.Apply {
		tmpDir := etcdDir + "/tmp"
		_ = executor.Run("mkdir", "-p", tmpDir)
		_ = executor.Run("tar", "-C", tmpDir, "-xzf", tgzPath)
		_ = executor.Run("cp", fmt.Sprintf("%s/etcd-v%s-linux-amd64/etcdctl", tmpDir, etcdVersion), "/usr/local/bin/")
		_ = executor.Run("chmod", "+x", "/usr/local/bin/etcdctl")
		_ = executor.Run("rm", "-rf", tmpDir)
	}

	return changes, nil
}

func desiredEtcdctlVersion(cfg config.Config) string {
	etcdVersion := strings.TrimPrefix(etcdTag(cfg), "v")
	if i := strings.LastIndex(etcdVersion, "-"); i > 0 {
		etcdVersion = etcdVersion[:i]
	}
	return etcdVersion
}

func etcdctlVersionMatches(executor *host.Executor, desiredVersion string) bool {
	out, err := executor.RunCapture("/usr/local/bin/etcdctl", "version")
	if err != nil {
		return false
	}
	return strings.Contains(out, "etcdctl version: "+desiredVersion)
}

func etcdHealthy(executor *host.Executor, endpoint, certDir string, tlsDisabled bool) bool {
	// Single attempt — this is detection ("does a cluster exist?"), not
	// convergence. A healthy cluster responds on the first try; retrying a
	// dead endpoint just wastes ~16s on new-cluster creation.
	return etcdHealthyOnce(executor, endpoint, certDir, tlsDisabled, "5s")
}

func checkMembership(executor *host.Executor, endpoint, certDir string, tlsDisabled bool, instanceName, nodeIP string) bool {
	args := etcdctlArgs(endpoint, certDir, tlsDisabled)
	args = append(args, "member", "list")
	out, err := runEtcdctl(executor, args...)
	if err != nil {
		return false
	}
	return strings.Contains(out, instanceName) || strings.Contains(out, nodeIP)
}

func skipMembershipReconcile(cfg config.Config) bool {
	return cfg.Operation() == config.OperationCARotate
}

// staticInitialClusterMembers returns the etcd initial-cluster member list used
// to bootstrap a NEW cluster without the (deprecated) v2 discovery service, and
// whether one could be determined. ETCD_INITIAL_CLUSTER (the full
// "name=peerURL,..." list) wins when set; otherwise a first/single master
// bootstraps a one-node cluster from its own peer URL (which then grows via
// member-add as other masters join through the LB). A non-first master with
// neither a member list nor an existing cluster to join returns false —
// bootstrapping itself would split-brain the cluster.
func staticInitialClusterMembers(cfg config.Config, nodeIP, protocol string) (string, bool) {
	if ic := strings.TrimSpace(cfg.Master.InitialCluster); ic != "" {
		return ic, true
	}
	if cfg.IsFirstMaster() {
		return fmt.Sprintf("%s=%s://%s:2380", cfg.Shared.InstanceName, protocol, nodeIP), true
	}
	return "", false
}

// needsSeedWait reports whether this node must wait for master-0's seed etcd to
// appear via the LB before it can join. True only for a non-first master that
// has no reachable LB cluster, no local etcd, is not already a member, and has
// no static ETCD_INITIAL_CLUSTER to bootstrap from. In that state bootstrapping
// locally would split-brain the cluster, so the only safe move is to wait.
func needsSeedWait(cfg config.Config, lbOK, localOK, isMember bool) bool {
	if cfg.Master == nil {
		return false
	}
	return !lbOK && !localOK && !isMember &&
		!cfg.IsFirstMaster() && strings.TrimSpace(cfg.Master.InitialCluster) == ""
}

// waitForSeedEtcd blocks until the etcd LB reports a healthy backend (master-0's
// seed cluster) or the bound elapses, returning true once healthy. Used by
// non-first masters on a fresh multi-master create where master-0 may still be
// forming the seed cluster. The bound (~10min) stays within the Heat deployment
// timeout; on exhaustion the caller falls through to the static-bootstrap branch
// which fails with a clear "seed never appeared" error.
func waitForSeedEtcd(executor *host.Executor, lbEndpoint, certDir string, tlsDisabled bool) bool {
	const attempts = 60
	const delay = 10 * time.Second
	for i := 0; i < attempts; i++ {
		if etcdHealthyOnce(executor, lbEndpoint, certDir, tlsDisabled, "5s") {
			if executor.Logger != nil {
				executor.Logger.Infof("etcd: seed cluster healthy via LB %s after %d wait attempt(s); proceeding to join", lbEndpoint, i+1)
			}
			return true
		}
		if executor.Logger != nil && i%6 == 0 {
			executor.Logger.Infof("etcd: waiting for master-0 seed cluster via LB %s (attempt %d/%d)", lbEndpoint, i+1, attempts)
		}
		if executor.Apply {
			time.Sleep(delay)
		}
	}
	if executor.Logger != nil {
		executor.Logger.Warnf("etcd: seed cluster did not appear via LB %s within bound; falling through to static bootstrap (will fail for a non-first master)", lbEndpoint)
	}
	return false
}

// etcdClusterToken returns a cluster-wide token so that every member of a
// freshly bootstrapped static cluster agrees it belongs to the same cluster.
func etcdClusterToken(cfg config.Config) string {
	if cfg.Shared.ClusterUUID != "" {
		return "etcd-" + cfg.Shared.ClusterUUID
	}
	return "magnum-etcd-cluster"
}

func rejoinCluster(cfg config.Config, executor *host.Executor, nodeIP, protocol, certDir, lbEndpoint string) ([]host.Change, error) {
	// Remove ourselves and re-add.
	args := etcdctlArgs(lbEndpoint, certDir, cfg.Shared.TLSDisabled)
	out, err := runEtcdctl(executor, append(args, "member", "list")...)
	if err != nil {
		// During initial cluster formation or quorum loss, member list can
		// transiently fail. Log and skip the remove step — the subsequent
		// member add will still work if the cluster accepts us.
		executor.Logger.Warnf("etcd rejoin: member list failed (cluster may be forming quorum), skipping remove: %v", err)
	} else {
		lines := strings.Split(out, "\n")
		for _, line := range lines {
			if strings.Contains(line, cfg.Shared.InstanceName) || strings.Contains(line, nodeIP) {
				parts := strings.SplitN(line, ",", 2)
				if len(parts) > 0 {
					memberID := strings.TrimSpace(parts[0])
					if _, rmErr := runEtcdctl(executor, append(args, "member", "remove", memberID)...); rmErr != nil {
						executor.Logger.Warnf("etcd rejoin: member remove failed (member %s, may be transient): %v", memberID, rmErr)
					}
				}
			}
		}
	}

	// Add back, retrying the transient strict-reconfig-check rejection that
	// occurs while the cluster is still forming quorum or other members are
	// starting.
	peerURL := protocol + "://" + nodeIP + ":2380"
	addOut, err := memberAddWithRetry(executor, args, cfg.Shared.InstanceName, peerURL)
	if err != nil {
		return nil, fmt.Errorf("rejoin etcd cluster: %w", err)
	}

	initialCluster := extractInitialCluster(addOut)
	if initialCluster == "" {
		// Member already existed (adopted slot) — derive the list from the cluster.
		initialCluster = currentInitialCluster(executor, lbEndpoint, certDir, cfg.Shared.TLSDisabled)
	}
	if initialCluster == "" {
		return nil, fmt.Errorf("rejoin etcd cluster: could not determine initial-cluster after member add")
	}
	conf := buildConfig(cfg, nodeIP, protocol, "existing", initialCluster)
	if err := clearEtcdData(executor); err != nil {
		return nil, err
	}
	return writeAndStartEtcd(executor, conf, protocol, nodeIP, certDir, cfg.Shared.TLSDisabled, true)
}

func joinExistingCluster(cfg config.Config, executor *host.Executor, nodeIP, protocol, certDir, lbEndpoint string) ([]host.Change, error) {
	args := etcdctlArgs(lbEndpoint, certDir, cfg.Shared.TLSDisabled)
	peerURL := protocol + "://" + nodeIP + ":2380"

	// Remove any stale member that carries our name but a different peer URL.
	// This happens when a node is recreated/auto-healed under the same name with
	// a new IP, or rebuilt after disk loss: the old entry is a dead voter that
	// degrades quorum and must go before we add ourselves afresh.
	removeStaleSelfMembers(cfg, executor, args, nodeIP, protocol)

	// Add ourselves, retrying the transient strict-reconfig-check rejection
	// ("not enough started members") that occurs during a parallel create or a
	// resize while peers are still starting.
	addOut, err := memberAddWithRetry(executor, args, cfg.Shared.InstanceName, peerURL)
	if err != nil {
		return nil, fmt.Errorf("join etcd cluster: %w", err)
	}

	initialCluster := extractInitialCluster(addOut)
	if initialCluster == "" {
		// Member already existed (pre-created/ghost slot) — derive the list from
		// the live cluster and start with state=existing to consume that slot.
		initialCluster = currentInitialCluster(executor, lbEndpoint, certDir, cfg.Shared.TLSDisabled)
	}
	if initialCluster == "" {
		return nil, fmt.Errorf("join etcd cluster: could not determine initial-cluster after member add")
	}
	conf := buildConfig(cfg, nodeIP, protocol, "existing", initialCluster)
	if err := clearEtcdData(executor); err != nil {
		return nil, err
	}
	return writeAndStartEtcd(executor, conf, protocol, nodeIP, certDir, cfg.Shared.TLSDisabled, true)
}

func cleanupEtcd(executor *host.Executor) {
	_ = executor.Run("systemctl", "stop", "etcd")
	_ = executor.Run("podman", "rm", "-f", "etcd")
	// Match bash behavior: wait for etcd to fully stop before proceeding.
	if executor.Apply {
		time.Sleep(5 * time.Second)
	}
}

func clearEtcdData(executor *host.Executor) error {
	if executor.Logger != nil {
		executor.Logger.Infof("etcd: clearing stale data dir /var/lib/etcd/default.etcd before member join")
	}
	if !executor.Apply {
		return nil
	}
	if err := os.RemoveAll("/var/lib/etcd/default.etcd"); err != nil {
		return fmt.Errorf("clear stale etcd data dir: %w", err)
	}
	return nil
}

// walPresent reports whether etcd has a populated write-ahead log, i.e. real
// persisted raft state. This is the authoritative signal that this node is an
// existing cluster member whose data must never be wiped or re-bootstrapped.
// Decisions key on this rather than on config-file/health probes, which can be
// transiently misleading (e.g. a healthy node whose etcd is merely slow to
// start after a reboot).
func walPresent() bool {
	entries, err := os.ReadDir("/var/lib/etcd/default.etcd/member/wal")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".wal") {
			return true
		}
	}
	return false
}

// startWithExistingData starts etcd for a node that already has a populated WAL.
// etcd ignores the initial-cluster* fields when a data dir is present and
// recovers membership from the WAL, so this path never wipes data. It writes a
// config only when one is missing (e.g. a rebuilt node whose etcd Cinder volume
// was reattached but whose root filesystem is fresh); otherwise it just ensures
// the service is active, leaving a healthy node untouched for idempotency.
// A returned error means etcd has data but would not become active — the caller
// decides whether the data is orphaned (wipe+rejoin) or this is a quorum-loss
// situation to fail safe on.
func startWithExistingData(cfg config.Config, executor *host.Executor, nodeIP, protocol, certDir, lbEndpoint string, lbOK bool) ([]host.Change, error) {
	if _, err := os.Stat("/etc/etcd/etcd.conf.yaml"); err != nil {
		// Config missing but data present: regenerate an existing-mode config.
		// The initial-cluster value is informational here (etcd loads membership
		// from the WAL); prefer the live member list when reachable, else fall
		// back to our own peer URL. Crucially, do NOT clear the data dir.
		initialCluster := fmt.Sprintf("%s=%s://%s:2380", cfg.Shared.InstanceName, protocol, nodeIP)
		if lbOK {
			if live := currentInitialCluster(executor, lbEndpoint, certDir, cfg.Shared.TLSDisabled); live != "" {
				initialCluster = live
			}
		}
		conf := buildConfig(cfg, nodeIP, protocol, "existing", initialCluster)
		return writeAndStartEtcd(executor, conf, protocol, nodeIP, certDir, cfg.Shared.TLSDisabled, false)
	}

	// Config present: rebuild only for TLS/proxy drift, then ensure active. Never
	// rewrite the membership config on a healthy node — that would restart etcd
	// on every periodic reconcile.
	if err := rebuildConfigIfNeeded(cfg, executor, nodeIP, protocol, certDir); err != nil {
		return nil, err
	}
	if executor.SystemctlIsActive("etcd") {
		return nil, nil
	}
	res, err := (hostresource.SystemdServiceSpec{Unit: "etcd", SkipIfMissing: true, DaemonReload: true, Active: hostresource.BoolPtr(true)}).Apply(executor)
	if err != nil {
		return res.Changes, fmt.Errorf("start etcd with existing data: %w", err)
	}
	if executor.Apply && !executor.WaitForSystemctlActive("etcd", 2*time.Minute, 2*time.Second) {
		return res.Changes, fmt.Errorf("etcd did not become active with existing data")
	}
	return res.Changes, nil
}

// memberAddWithRetry runs `etcdctl member add` for this node, retrying on the
// transient errors seen during concurrent cluster formation and resize. The
// canonical one is "etcdserver: re-configuration failed due to not enough
// started members": with --strict-reconfig-check, adding the Nth voting member
// is rejected until N-1 members are actually started. On a parallel create
// several masters race to add themselves, so this is expected and self-clears
// once peers finish booting — it must be retried, not treated as fatal. If the
// member already exists (a pre-created/ghost slot, or a lost ack), that is
// treated as success and an empty output is returned so the caller derives the
// member list from the live cluster. The bound (~12 × 10s) stays well within
// Heat's deployment timeout. RunCapture is called directly (not runEtcdctl) to
// avoid runEtcdctl's inner retry re-issuing this non-idempotent command.
func memberAddWithRetry(executor *host.Executor, args []string, instanceName, peerURL string) (string, error) {
	addArgs := make([]string, 0, len(args)+4)
	addArgs = append(addArgs, args...)
	addArgs = append(addArgs, "member", "add", instanceName, "--peer-urls="+peerURL)

	const attempts = 12
	const delay = 10 * time.Second
	var lastErr error
	for i := 0; i < attempts; i++ {
		out, err := executor.RunCapture("/usr/local/bin/etcdctl", addArgs...)
		if err == nil {
			return out, nil
		}
		if isAlreadyMemberErr(err) {
			if executor.Logger != nil {
				executor.Logger.Infof("etcd: member %s already present; adopting existing slot", instanceName)
			}
			return "", nil
		}
		lastErr = err
		if !isTransientMemberAddErr(err) {
			return "", err
		}
		if executor.Logger != nil {
			executor.Logger.Warnf("etcd: member add for %s transient failure (attempt %d/%d), retrying in %s: %v", instanceName, i+1, attempts, delay, err)
		}
		if !executor.Apply {
			break
		}
		time.Sleep(delay)
	}
	return "", fmt.Errorf("member add for %s did not succeed after %d attempts: %w", instanceName, attempts, lastErr)
}

// isTransientMemberAddErr reports whether a member-add failure is the kind that
// clears once peers finish starting or a leader settles, and so should be
// retried rather than failing the phase. RunCapture embeds etcd's stderr in the
// error string, so substring matching is reliable.
func isTransientMemberAddErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"not enough started members",
		"unhealthy cluster",
		"request timed out",
		"context deadline exceeded",
		"leader changed",
		"no leader",
		"connection refused",
		"too many requests",
		"rpc error",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// isAlreadyMemberErr reports whether a member-add failed because the member (or
// its peer URL) is already registered — meaning the add effectively took.
func isAlreadyMemberErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "peer urls already exists") ||
		strings.Contains(msg, "member already exists")
}

// removeStaleSelfMembers removes any etcd member that shares this node's name
// but advertises a different peer URL — the leftover entry from a previous
// incarnation of this master (recreated/auto-healed under the same name with a
// new IP, or rebuilt after disk loss). Such an entry is an unreachable voter
// that degrades quorum and must go before adding the fresh node. Best-effort:
// failures are logged and tolerated so the subsequent add still proceeds.
func removeStaleSelfMembers(cfg config.Config, executor *host.Executor, args []string, nodeIP, protocol string) {
	myPeer := protocol + "://" + nodeIP + ":2380"
	out, err := runEtcdctl(executor, append(append([]string{}, args...), "member", "list")...)
	if err != nil {
		return
	}
	for _, m := range parseMemberList(out) {
		if m.name != cfg.Shared.InstanceName || m.peerURL == myPeer {
			continue
		}
		if _, rmErr := runEtcdctl(executor, append(append([]string{}, args...), "member", "remove", m.id)...); rmErr != nil {
			if executor.Logger != nil {
				executor.Logger.Warnf("etcd: failed to remove stale self member %s (old peer %s); continuing: %v", m.id, m.peerURL, rmErr)
			}
		} else if executor.Logger != nil {
			executor.Logger.Infof("etcd: removed stale self member %s (old peer %s) before rejoining as %s", m.id, m.peerURL, myPeer)
		}
	}
}

// currentInitialCluster builds a "name=peerURL,..." string from the live etcd
// member list. Members not yet started (empty name) are skipped. Returns "" if
// the list cannot be read or parsed.
func currentInitialCluster(executor *host.Executor, endpoint, certDir string, tlsDisabled bool) string {
	args := etcdctlArgs(endpoint, certDir, tlsDisabled)
	out, err := runEtcdctl(executor, append(args, "member", "list")...)
	if err != nil {
		return ""
	}
	var entries []string
	for _, m := range parseMemberList(out) {
		if m.name == "" || m.peerURL == "" {
			continue
		}
		entries = append(entries, m.name+"="+m.peerURL)
	}
	return strings.Join(entries, ",")
}

// etcdMember is one row of `etcdctl member list` default CSV output.
type etcdMember struct {
	id      string
	name    string
	peerURL string
}

// parseMemberList parses default `etcdctl member list` CSV output
// (ID, status, name, peerURLs, clientURLs, isLearner) into members. Lines with
// fewer than 4 fields are skipped.
func parseMemberList(out string) []etcdMember {
	var ms []etcdMember
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 4 {
			continue
		}
		ms = append(ms, etcdMember{
			id:      strings.TrimSpace(fields[0]),
			name:    strings.TrimSpace(fields[2]),
			peerURL: strings.TrimSpace(fields[3]),
		})
	}
	return ms
}

func cleanupExcessMembers(cfg config.Config, executor *host.Executor, protocol, certDir string) {
	if !strings.HasSuffix(cfg.Shared.InstanceName, "master-0") {
		return
	}
	if cfg.Master.NumberOfMasters == 0 {
		return
	}

	lbEndpoint := fmt.Sprintf("%s://%s:2379", protocol, cfg.Master.EtcdLBVIP)
	args := etcdctlArgs(lbEndpoint, certDir, cfg.Shared.TLSDisabled)
	out, err := runEtcdctl(executor, append(args, "member", "list")...)
	if err != nil {
		return
	}

	members := parseMemberList(out)
	if len(members) <= cfg.Master.NumberOfMasters {
		return
	}

	// Sort members by master index in reverse order (highest first) to maintain
	// quorum safety by removing the highest-numbered master first.
	type memberEntry struct {
		member    etcdMember
		masterIdx int
	}
	var candidates []memberEntry
	for _, m := range members {
		if strings.Contains(m.name, cfg.Shared.InstanceName) {
			continue
		}
		candidates = append(candidates, memberEntry{member: m, masterIdx: extractMasterIndex(m.name)})
	}

	// Sort by master index descending — consider highest-numbered first.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].masterIdx > candidates[j].masterIdx
	})

	// Remove ONE excess member per reconcile (quorum safety) — but only a member
	// that is genuinely gone. This function exists to evict the leftover member of
	// a SCALED-DOWN master whose VM was deleted; that member's etcd is
	// unreachable. It must NEVER evict a started, healthy member merely because
	// the local node count says "excess": on a master scale-up the existing
	// master-0 can carry a STALE NUMBER_OF_MASTERS (Heat does not always refresh
	// the existing master's heat-params on resize), and a healthy, freshly-joined
	// master would otherwise be culled — wedging it permanently (the removed
	// member can never rejoin with its data; master-0 then logs "rejected stream
	// ... because it was removed"). So probe each candidate's client endpoint and
	// remove only the highest-indexed one that is NOT healthy.
	for _, c := range candidates {
		clientEndpoint := memberClientEndpoint(c.member.peerURL)
		if clientEndpoint != "" && etcdHealthy(executor, clientEndpoint, certDir, cfg.Shared.TLSDisabled) {
			if executor.Logger != nil {
				executor.Logger.Infof("etcd: refusing to evict excess member %s (%s) — endpoint healthy; node count likely stale", c.member.name, c.member.id)
			}
			continue
		}
		if executor.Logger != nil {
			executor.Logger.Infof("etcd: removing excess member %s (%s, peer %s) — endpoint unreachable, treating as scaled-down orphan", c.member.name, c.member.id, c.member.peerURL)
		}
		runEtcdctl(executor, append(args, "member", "remove", c.member.id)...)
		return
	}
}

// memberClientEndpoint derives a member's client URL (port 2379) from its peer
// URL (port 2380), for health-probing a peer before deciding to evict it.
// Returns "" when the peer URL is empty/unparseable.
func memberClientEndpoint(peerURL string) string {
	if peerURL == "" {
		return ""
	}
	return strings.Replace(peerURL, ":2380", ":2379", 1)
}

// extractMasterIndex parses "master-N" from a member list line and returns N.
// Returns -1 if no master index is found.
func extractMasterIndex(memberLine string) int {
	// Member list lines contain the member name. Look for "master-N" pattern.
	idx := strings.Index(memberLine, "master-")
	if idx < 0 {
		return -1
	}
	rest := memberLine[idx+len("master-"):]
	// Extract the numeric part.
	numStr := ""
	for _, ch := range rest {
		if ch >= '0' && ch <= '9' {
			numStr += string(ch)
		} else {
			break
		}
	}
	if numStr == "" {
		return -1
	}
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return -1
	}
	return n
}

func writeAndStartEtcd(executor *host.Executor, config, protocol, nodeIP, certDir string, tlsDisabled bool, waitForEndpointHealth bool) ([]host.Change, error) {
	dirResult, err := (hostresource.DirectorySpec{Path: "/etc/etcd", Mode: 0o755}).Apply(executor)
	if err != nil {
		return nil, err
	}
	var changes []host.Change
	changes = append(changes, dirResult.Changes...)

	fileResult, err := (hostresource.FileSpec{Path: "/etc/etcd/etcd.conf.yaml", Content: []byte(config), Mode: 0o644}).Apply(executor)
	if err != nil {
		return nil, err
	}
	configChanged := fileResult.Changed
	changes = append(changes, fileResult.Changes...)

	started := false
	if configChanged {
		serviceResult, err := (hostresource.SystemdServiceSpec{Unit: "etcd", SkipIfMissing: true, DaemonReload: true, Restart: true, RestartReason: "etcd config changed"}).Apply(executor)
		if err != nil {
			return nil, fmt.Errorf("restart etcd: %w", err)
		}
		changes = append(changes, serviceResult.Changes...)
		started = true
	} else if !executor.SystemctlIsActive("etcd") {
		// Drift: etcd should be running but isn't.
		serviceResult, err := (hostresource.SystemdServiceSpec{Unit: "etcd", SkipIfMissing: true, DaemonReload: true, Active: hostresource.BoolPtr(true)}).Apply(executor)
		if err != nil {
			return nil, fmt.Errorf("start etcd: %w", err)
		}
		changes = append(changes, serviceResult.Changes...)
		started = true
	}

	if started && executor.Apply {
		if !executor.WaitForSystemctlActive("etcd", 2*time.Minute, 2*time.Second) {
			return nil, fmt.Errorf("etcd service did not become active after start")
		}
		if !waitForEndpointHealth {
			if executor.Logger != nil {
				executor.Logger.Infof("etcd: service is active; skipping immediate endpoint health wait while the multi-member cluster forms")
			}
			return changes, nil
		}

		// Wait for etcd to be functionally healthy after start when this node
		// is joining an existing cluster or forming a single-master cluster.
		// For a multi-master static bootstrap, endpoint health may remain false
		// until enough peer members have started to elect quorum.
		healthy := false
		localEP := fmt.Sprintf("%s://%s:2379", protocol, nodeIP)
		for i := 0; i < 60; i++ {
			if etcdHealthyOnce(executor, localEP, certDir, tlsDisabled, "2s") {
				healthy = true
				break
			}
			time.Sleep(3 * time.Second)
		}
		if !healthy {
			return nil, fmt.Errorf("etcd did not become healthy within 5 minutes after start")
		}
	}

	return changes, nil
}

func rebuildConfigIfNeeded(cfg config.Config, executor *host.Executor, nodeIP, protocol, certDir string) error {
	data, err := os.ReadFile("/etc/etcd/etcd.conf.yaml")
	if err != nil {
		return nil
	}

	needsTLS := !cfg.Shared.TLSDisabled && !strings.Contains(string(data), "client-transport-security")
	if !needsTLS {
		return nil
	}

	// Determine mode and rebuild.
	content := string(data)
	configChanged := false
	if strings.Contains(content, "initial-cluster:") {
		// Extract initial-cluster value.
		for _, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "initial-cluster:") {
				ic := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "initial-cluster:"))
				ic = strings.Trim(ic, "\"")
				conf := buildConfig(cfg, nodeIP, protocol, "existing", ic)
				result, err := (hostresource.FileSpec{Path: "/etc/etcd/etcd.conf.yaml", Content: []byte(conf), Mode: 0o644}).Apply(executor)
				if err != nil {
					return fmt.Errorf("rebuild etcd config (existing cluster mode): %w", err)
				}
				configChanged = result.Changed
				break
			}
		}
	}

	if configChanged {
		_, _ = (hostresource.SystemdServiceSpec{Unit: "etcd", SkipIfMissing: true, DaemonReload: true, Restart: true, RestartReason: "rebuilt etcd config"}).Apply(executor)
	}
	return nil
}

func buildConfig(cfg config.Config, nodeIP, protocol, mode, initialCluster string) string {
	certDir := "/etc/etcd/certs"
	var b strings.Builder

	fmt.Fprintf(&b, "name: \"%s\"\n", cfg.Shared.InstanceName)
	fmt.Fprintf(&b, "data-dir: \"/var/lib/etcd/default.etcd\"\n")
	fmt.Fprintf(&b, "listen-metrics-urls: \"http://%s:2378\"\n", nodeIP)
	fmt.Fprintf(&b, "listen-client-urls: \"%s://%s:2379,http://127.0.0.1:2379\"\n", protocol, nodeIP)
	fmt.Fprintf(&b, "listen-peer-urls: \"%s://%s:2380\"\n", protocol, nodeIP)
	fmt.Fprintf(&b, "advertise-client-urls: \"%s://%s:2379\"\n", protocol, nodeIP)
	fmt.Fprintf(&b, "initial-advertise-peer-urls: \"%s://%s:2380\"\n", protocol, nodeIP)

	switch mode {
	case "new-static":
		// Discovery-free bootstrap of a brand-new cluster from a known member
		// list. initial-cluster-state "new" and a shared token form one cluster.
		fmt.Fprintf(&b, "initial-cluster: \"%s\"\n", initialCluster)
		fmt.Fprintf(&b, "initial-cluster-state: \"new\"\n")
		fmt.Fprintf(&b, "initial-cluster-token: \"%s\"\n", etcdClusterToken(cfg))
	default:
		// "existing": joining a cluster that already has members.
		fmt.Fprintf(&b, "initial-cluster: \"%s\"\n", initialCluster)
		fmt.Fprintf(&b, "initial-cluster-state: \"existing\"\n")
	}

	fmt.Fprintf(&b, "heartbeat-interval: 1000\n")
	fmt.Fprintf(&b, "election-timeout: 15000\n")
	fmt.Fprintf(&b, "auto-compaction-mode: periodic\n")
	fmt.Fprintf(&b, "auto-compaction-retention: \"24h\"\n")

	// TLS configuration.
	if !cfg.Shared.TLSDisabled {
		fmt.Fprintf(&b, "client-transport-security:\n")
		fmt.Fprintf(&b, "  cert-file: \"%s/server.crt\"\n", certDir)
		fmt.Fprintf(&b, "  key-file: \"%s/server.key\"\n", certDir)
		fmt.Fprintf(&b, "  client-cert-auth: true\n")
		fmt.Fprintf(&b, "  trusted-ca-file: \"%s/ca.crt\"\n", certDir)
		fmt.Fprintf(&b, "peer-transport-security:\n")
		fmt.Fprintf(&b, "  cert-file: \"%s/server.crt\"\n", certDir)
		fmt.Fprintf(&b, "  key-file: \"%s/server.key\"\n", certDir)
		fmt.Fprintf(&b, "  client-cert-auth: true\n")
		fmt.Fprintf(&b, "  trusted-ca-file: \"%s/ca.crt\"\n", certDir)
	}

	return b.String()
}

func etcdctlArgs(endpoint, certDir string, tlsDisabled bool) []string {
	return etcdctlArgsWithTimeout(endpoint, certDir, tlsDisabled, "5s")
}

func etcdctlArgsWithTimeout(endpoint, certDir string, tlsDisabled bool, timeout string) []string {
	if timeout == "" {
		timeout = "5s"
	}
	args := []string{"--endpoints=" + endpoint, "--command-timeout=" + timeout}
	if !tlsDisabled {
		args = append(args,
			"--cacert="+certDir+"/ca.crt",
			"--cert="+certDir+"/server.crt",
			"--key="+certDir+"/server.key",
		)
	}
	return args
}

func etcdHealthyOnce(executor *host.Executor, endpoint, certDir string, tlsDisabled bool, timeout string) bool {
	args := etcdctlArgsWithTimeout(endpoint, certDir, tlsDisabled, timeout)
	args = append(args, "endpoint", "health")
	_, err := executor.RunCapture("/usr/local/bin/etcdctl", args...)
	return err == nil
}

// runEtcdctl runs etcdctl with retry logic (3 attempts, 3s delay),
// matching bash's run_etcdctl function.
// Note: ETCDCTL_API=3 is not set because etcd 3.5+ removed the v2 API
// entirely — v3 is the only API and the env var is unrecognized.
func runEtcdctl(executor *host.Executor, args ...string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		out, err := executor.RunCapture("/usr/local/bin/etcdctl", args...)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if attempt < 2 && executor.Apply {
			time.Sleep(3 * time.Second)
		}
	}
	return "", lastErr
}

func extractInitialCluster(addOutput string) string {
	for _, line := range strings.Split(addOutput, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ETCD_INITIAL_CLUSTER=") {
			val := strings.TrimPrefix(line, "ETCD_INITIAL_CLUSTER=")
			return strings.Trim(val, "\"")
		}
	}
	return ""
}

// Destroy removes this node from the etcd cluster and stops the etcd service.
// Called during bootstrap destroy in reverse phase order.
func (Module) Destroy(_ context.Context, cfg config.Config, req moduleapi.Request) error {
	if cfg.Master == nil {
		return nil // not a master, nothing to do
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	nodeIP := cfg.ResolveNodeIP()
	certDir := "/etc/kubernetes"

	protocol := "https"
	if cfg.Shared.TLSDisabled {
		protocol = "http"
	}

	// Try to remove self from etcd cluster via the LB endpoint.
	lbEndpoint := fmt.Sprintf("%s://%s:2379", protocol, cfg.Master.EtcdLBVIP)
	args := etcdctlArgs(lbEndpoint, certDir, cfg.Shared.TLSDisabled)
	out, err := runEtcdctl(executor, append(args, "member", "list")...)
	if err != nil {
		if req.Logger != nil {
			req.Logger.Warnf("etcd destroy: member list failed: %v (etcd may already be down)", err)
		}
	} else {
		// Find our member ID and remove ourselves.
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, cfg.Shared.InstanceName) || strings.Contains(line, nodeIP) {
				parts := strings.SplitN(line, ",", 2)
				if len(parts) > 0 {
					memberID := strings.TrimSpace(parts[0])
					if req.Logger != nil {
						req.Logger.Infof("etcd destroy: removing member=%s from cluster", memberID)
					}
					removeArgs := etcdctlArgs(lbEndpoint, certDir, cfg.Shared.TLSDisabled)
					_, _ = runEtcdctl(executor, append(removeArgs, "member", "remove", memberID)...)
				}
				break
			}
		}
	}

	// Stop etcd service.
	_, _ = (hostresource.SystemdServiceSpec{Unit: "etcd", SkipIfMissing: true, Active: hostresource.BoolPtr(false), Enabled: hostresource.BoolPtr(false)}).Apply(executor)

	// Clean etcd data directory.
	etcdDataDir := "/var/lib/etcd"
	if req.Logger != nil {
		req.Logger.Infof("etcd destroy: cleaning data dir=%s", etcdDataDir)
	}
	_ = executor.Run("rm", "-rf", etcdDataDir)

	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:Etcd", name, res, opts...); err != nil {
		return nil, err
	}
	if cfg.Master != nil {
		childOpts := hostresource.ChildResourceOptions(res, opts...)
		executor := host.NewExecutor(false, nil)
		if cfg.Master.EtcdVolumeSize > 0 {
			dataDirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-data-dir", hostresource.DirectorySpec{Path: "/var/lib/etcd", Mode: 0o755}, childOpts...)
			if err != nil {
				return nil, err
			}
			if devicePath, err := findEtcdVolumeDevice(cfg, executor); err == nil && devicePath != "" {
				fstabLine := fmt.Sprintf("%s /var/lib/etcd xfs defaults 0 0", devicePath)
				fstabOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, dataDirRes)
				if _, err := hostsdk.RegisterLineSpec(ctx, name+"-fstab", hostresource.LineSpec{Path: "/etc/fstab", Line: fstabLine, Mode: 0o644}, fstabOpts...); err != nil {
					return nil, err
				}
			}
		}
		if cfg.Shared.UsePodman {
			if _, err := hostsdk.RegisterFileSpec(ctx, name+"-service-file", hostresource.FileSpec{Path: "/etc/systemd/system/etcd.service", Content: []byte(buildEtcdService(cfg)), Mode: 0o644}, childOpts...); err != nil {
				return nil, err
			}
		}
		if etcdVersion := desiredEtcdctlVersion(cfg); etcdVersion != "" {
			etcdDir := "/srv/magnum/etcd"
			tgzURL := fmt.Sprintf("https://github.com/etcd-io/etcd/releases/download/v%s/etcd-v%s-linux-amd64.tar.gz", etcdVersion, etcdVersion)
			tgzPath := fmt.Sprintf("%s/etcd-v%s-linux-amd64.tar.gz", etcdDir, etcdVersion)
			etcdDirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-etcdctl-dir", hostresource.DirectorySpec{Path: etcdDir, Mode: 0o755}, childOpts...)
			if err != nil {
				return nil, err
			}
			downloadOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, etcdDirRes)
			if _, err := hostsdk.RegisterDownloadSpec(ctx, name+"-etcdctl-download", hostresource.DownloadSpec{URL: tgzURL, Path: tgzPath, Mode: 0o644, Retries: 5}, downloadOpts...); err != nil {
				return nil, err
			}
		}
		configDirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-config-dir", hostresource.DirectorySpec{Path: "/etc/etcd", Mode: 0o755}, childOpts...)
		if err != nil {
			return nil, err
		}
		if data, err := os.ReadFile("/etc/etcd/etcd.conf.yaml"); err == nil {
			configOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, configDirRes)
			if _, err := hostsdk.RegisterFileSpec(ctx, name+"-config-file", hostresource.FileSpec{Path: "/etc/etcd/etcd.conf.yaml", Content: data, Mode: 0o644}, configOpts...); err != nil {
				return nil, err
			}
		}
	}
	outputs := pulumi.Map{
		"role": pulumi.String(cfg.Role().String()),
	}
	if cfg.Master != nil {
		outputs["etcdTag"] = pulumi.String(etcdTag(cfg))
	}
	if err := ctx.RegisterResourceOutputs(res, outputs); err != nil {
		return nil, err
	}
	return res, nil
}

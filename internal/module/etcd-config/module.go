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
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

// etcdImageTags maps K8s minor version to the etcd image tag that kubeadm bundles.
// Source: kubernetes/kubernetes cmd/kubeadm/app/constants/constants.go
var etcdImageTags = map[string]string{
	"1.35": "3.6.10-0",
	"1.34": "3.6.5-0",
	"1.33": "3.5.24-0",
	"1.32": "3.5.24-0",
	"1.31": "3.5.24-0",
	"1.30": "3.5.15-0",
	"1.29": "3.5.16-0",
	"1.28": "3.5.15-0",
	"1.27": "3.5.12-0",
	"1.26": "3.5.10-0",
	"1.25": "3.5.9-0",
	"1.24": "3.5.6-0",
	"1.23": "3.5.6-0",
	"1.22": "3.5.6-0",
	"1.21": "3.4.13-0",
	"1.20": "3.4.13-0",
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

	// Resize operation: only cleanup excess members.
	if cfg.Shared.IsResize {
		cleanupExcessMembers(cfg, executor, protocol, certDir)
		return moduleapi.Result{
			Changes: changes,
			Outputs: map[string]string{"etcdTag": etcdTag(cfg)},
		}, nil
	}

	// Cluster join/creation logic.
	lbEndpoint := fmt.Sprintf("%s://%s:2379", protocol, cfg.Master.EtcdLBVIP)
	localEndpoint := fmt.Sprintf("%s://%s:2379", protocol, nodeIP)

	// Always check the LB endpoint — an existing cluster may be running
	// (e.g. scaling 1→2 masters) even if this node has never had etcd.
	// Only skip the LOCAL endpoint check when there's no config — local
	// etcd isn't running so the check just wastes time on retries.
	lbOK := etcdHealthy(executor, lbEndpoint, certDir, cfg.Shared.TLSDisabled)
	localOK := false
	isMember := false
	_, configErr := os.Stat("/etc/etcd/etcd.conf.yaml")
	if configErr == nil {
		localOK = etcdHealthy(executor, localEndpoint, certDir, cfg.Shared.TLSDisabled)
	}
	if lbOK {
		isMember = checkMembership(executor, lbEndpoint, certDir, cfg.Shared.TLSDisabled, cfg.Shared.InstanceName, nodeIP)
	}
	discoveryOK := checkDiscoveryURL(executor, cfg.Master.EtcdDiscoveryURL)
	if req.Logger != nil {
		req.Logger.Infof("etcd: discoveryOK=%t lbOK=%t localOK=%t isMember=%t lbEndpoint=%s localEndpoint=%s",
			discoveryOK, lbOK, localOK, isMember, lbEndpoint, localEndpoint)
	}

	switch {
	case isMember && localOK:
		// Already healthy member — rebuild config if needed for TLS/proxy.
		if err := rebuildConfigIfNeeded(cfg, executor, nodeIP, protocol, certDir); err != nil {
			return moduleapi.Result{}, err
		}

	case isMember && !localOK:
		// Member but unhealthy — rejoin.
		cs, err := rejoinCluster(cfg, executor, nodeIP, protocol, certDir, lbEndpoint)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)

	case !isMember && lbOK:
		// LB has cluster but we're not a member — join.
		cleanupEtcd(executor)
		cs, err := joinExistingCluster(cfg, executor, nodeIP, protocol, certDir, lbEndpoint)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)

	case discoveryOK:
		// New cluster via discovery URL.
		cleanupEtcd(executor)
		etcdConf := buildConfig(cfg, nodeIP, protocol, "new", "")
		waitForEndpointHealth := discoveryEndpointHealthRequired(cfg)
		cs, err := writeAndStartEtcd(executor, etcdConf, protocol, nodeIP, certDir, cfg.Shared.TLSDisabled, waitForEndpointHealth)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)

	default:
		return moduleapi.Result{}, fmt.Errorf("etcd: no valid discovery URL or healthy LB endpoint")
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
	prefix := volume
	if len(prefix) > 20 {
		prefix = prefix[:20]
	}

	// Wait for the device to appear, matching bash behavior:
	// retry up to 60 times with udevadm trigger and 0.5s sleep.
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
			break
		}
		if executor.Apply {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if devicePath == "" {
		return nil, fmt.Errorf("etcd volume device with prefix %q never appeared in /dev/disk/by-id after 60 attempts", prefix)
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

	change, err := executor.EnsureDir("/var/lib/etcd", 0o755)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	fstabLine := fmt.Sprintf("%s /var/lib/etcd xfs defaults 0 0", devicePath)
	change, err = executor.EnsureLine("/etc/fstab", fstabLine, 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
		_ = executor.Run("mount", "-a")
		_ = executor.Run("chown", "-R", "etcd.etcd", "/var/lib/etcd")
		_ = executor.Run("chmod", "755", "/var/lib/etcd")
	}

	return changes, nil
}

func writeEtcdService(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	if !cfg.Shared.UsePodman {
		return nil, nil
	}

	containerImage := cfg.Shared.ContainerInfraPrefix
	if containerImage == "" {
		containerImage = "registry.k8s.io/"
	}
	containerImage += "etcd"

	content := fmt.Sprintf(`[Unit]
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

	change, err := executor.EnsureFile("/etc/systemd/system/etcd.service", []byte(content), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		_ = executor.Run("systemctl", "daemon-reload")
		return []host.Change{*change}, nil
	}
	return nil, nil
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

	change, err := executor.EnsureDir(etcdDir, 0o755)
	if err != nil {
		return nil, err
	}
	var changes []host.Change
	if change != nil {
		changes = append(changes, *change)
	}

	dl, err := executor.DownloadFileWithRetry(context.Background(), tgzURL, tgzPath, 0o644, 5)
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
	args := etcdctlArgs(endpoint, certDir, tlsDisabled)
	args = append(args, "endpoint", "health")
	_, err := runEtcdctl(executor, args...)
	return err == nil
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

func checkDiscoveryURL(executor *host.Executor, url string) bool {
	if url == "" {
		return false
	}
	out, err := executor.RunCapture("curl", "-sf", url)
	if err != nil || out == "" {
		return false
	}
	if strings.Contains(out, "unable to GET token") {
		return false
	}
	// Validate that the response contains actual cluster data.
	if !strings.Contains(out, `"nodes":[`) && !strings.Contains(out, `"dir":true`) {
		return false
	}
	return true
}

func skipMembershipReconcile(cfg config.Config) bool {
	return cfg.Operation() == config.OperationCARotate
}

func discoveryEndpointHealthRequired(cfg config.Config) bool {
	return cfg.Master != nil && cfg.Master.NumberOfMasters == 1
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
						if cfg.Shared.IsResize {
							executor.Logger.Errorf("etcd rejoin: member remove failed during resize (member %s): %v", memberID, rmErr)
						} else {
							executor.Logger.Warnf("etcd rejoin: member remove failed (member %s, may be transient): %v", memberID, rmErr)
						}
					}
				}
			}
		}
	}

	// Add back.
	addArgs := append(args, "member", "add", cfg.Shared.InstanceName,
		"--peer-urls="+protocol+"://"+nodeIP+":2380")
	addOut, err := runEtcdctl(executor, addArgs...)
	if err != nil {
		return nil, fmt.Errorf("rejoin etcd cluster: %w", err)
	}

	initialCluster := extractInitialCluster(addOut)
	conf := buildConfig(cfg, nodeIP, protocol, "existing", initialCluster)
	if err := clearEtcdData(executor); err != nil {
		return nil, err
	}
	return writeAndStartEtcd(executor, conf, protocol, nodeIP, certDir, cfg.Shared.TLSDisabled, true)
}

func joinExistingCluster(cfg config.Config, executor *host.Executor, nodeIP, protocol, certDir, lbEndpoint string) ([]host.Change, error) {
	args := etcdctlArgs(lbEndpoint, certDir, cfg.Shared.TLSDisabled)
	addArgs := append(args, "member", "add", cfg.Shared.InstanceName,
		"--peer-urls="+protocol+"://"+nodeIP+":2380")
	addOut, err := runEtcdctl(executor, addArgs...)
	if err != nil {
		return nil, fmt.Errorf("join etcd cluster: %w", err)
	}

	initialCluster := extractInitialCluster(addOut)
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

	members := strings.Split(strings.TrimSpace(out), "\n")
	currentCount := len(members)
	if currentCount <= cfg.Master.NumberOfMasters {
		return
	}

	// Sort members by master index in reverse order (highest first) to maintain
	// quorum safety by removing the highest-numbered master first.
	type memberEntry struct {
		line      string
		memberID  string
		masterIdx int
	}
	var candidates []memberEntry
	for _, member := range members {
		if strings.Contains(member, cfg.Shared.InstanceName) {
			continue
		}
		parts := strings.SplitN(member, ",", 2)
		if len(parts) == 0 {
			continue
		}
		id := strings.TrimSpace(parts[0])
		idx := extractMasterIndex(member)
		candidates = append(candidates, memberEntry{line: member, memberID: id, masterIdx: idx})
	}

	// Sort by master index descending — remove highest-numbered first.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].masterIdx > candidates[j].masterIdx
	})

	// Remove one excess member at a time (quorum safety).
	if len(candidates) > 0 {
		runEtcdctl(executor, append(args, "member", "remove", candidates[0].memberID)...)
	}
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
	change, err := executor.EnsureDir("/etc/etcd", 0o755)
	if err != nil {
		return nil, err
	}
	var changes []host.Change
	if change != nil {
		changes = append(changes, *change)
	}

	change, err = executor.EnsureFile("/etc/etcd/etcd.conf.yaml", []byte(config), 0o644)
	if err != nil {
		return nil, err
	}
	configChanged := change != nil
	if change != nil {
		changes = append(changes, *change)
	}

	started := false
	if configChanged {
		_ = executor.Run("systemctl", "daemon-reload")
		if err := executor.Run("systemctl", "restart", "etcd"); err != nil {
			return nil, fmt.Errorf("restart etcd: %w", err)
		}
		changes = append(changes, host.Change{Action: host.ActionRestart, Summary: "restart etcd (config changed)"})
		started = true
	} else if !executor.SystemctlIsActive("etcd") {
		// Drift: etcd should be running but isn't.
		_ = executor.Run("systemctl", "daemon-reload")
		if err := executor.Run("systemctl", "start", "etcd"); err != nil {
			return nil, fmt.Errorf("start etcd: %w", err)
		}
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: "start etcd (was not running)"})
		started = true
	}

	if started && executor.Apply {
		if !executor.WaitForSystemctlActive("etcd", 2*time.Minute, 2*time.Second) {
			return nil, fmt.Errorf("etcd service did not become active after start")
		}
		if !waitForEndpointHealth {
			if executor.Logger != nil {
				executor.Logger.Infof("etcd: service is active; skipping immediate endpoint health wait while discovery cluster forms")
			}
			return changes, nil
		}

		// Wait for etcd to be functionally healthy after start when this node
		// is joining an existing cluster or forming a single-master cluster.
		// For multi-master discovery bootstrap, endpoint health may remain
		// false until enough peer members have started to elect quorum.
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
	needsProxy := cfg.Shared.HTTPProxy != "" && !strings.Contains(string(data), "discovery-proxy")

	if !needsTLS && !needsProxy {
		return nil
	}

	// Determine mode and rebuild.
	content := string(data)
	if strings.Contains(content, "discovery:") {
		conf := buildConfig(cfg, nodeIP, protocol, "new", "")
		if _, err := executor.EnsureFile("/etc/etcd/etcd.conf.yaml", []byte(conf), 0o644); err != nil {
			return fmt.Errorf("rebuild etcd config (discovery mode): %w", err)
		}
	} else if strings.Contains(content, "initial-cluster:") {
		// Extract initial-cluster value.
		for _, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "initial-cluster:") {
				ic := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "initial-cluster:"))
				ic = strings.Trim(ic, "\"")
				conf := buildConfig(cfg, nodeIP, protocol, "existing", ic)
				if _, err := executor.EnsureFile("/etc/etcd/etcd.conf.yaml", []byte(conf), 0o644); err != nil {
					return fmt.Errorf("rebuild etcd config (existing cluster mode): %w", err)
				}
				break
			}
		}
	}

	_ = executor.Run("systemctl", "daemon-reload")
	_ = executor.Run("systemctl", "restart", "etcd")
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

	if mode == "new" {
		fmt.Fprintf(&b, "discovery: \"%s\"\n", cfg.Master.EtcdDiscoveryURL)
	} else {
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

	// Proxy configuration.
	if cfg.Shared.HTTPProxy != "" {
		fmt.Fprintf(&b, "discovery-proxy: %s\n", cfg.Shared.HTTPProxy)
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
	_ = executor.Run("systemctl", "stop", "etcd")
	_ = executor.Run("systemctl", "disable", "etcd")

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
	outputs := pulumi.Map{
		"role": pulumi.String(cfg.Role().String()),
	}
	if cfg.Master != nil {
		outputs["etcdTag"] = pulumi.String(etcdTag(cfg))
		outputs["etcdDiscoveryUrl"] = pulumi.String(cfg.Master.EtcdDiscoveryURL)
	}
	if err := ctx.RegisterResourceOutputs(res, outputs); err != nil {
		return nil, err
	}
	return res, nil
}

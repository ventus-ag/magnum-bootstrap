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
			Outputs: map[string]string{"etcdTag": cfg.Master.EtcdTag},
		}, nil
	}

	// Cluster join/creation logic.
	lbEndpoint := fmt.Sprintf("%s://%s:2379", protocol, cfg.Master.EtcdLBVIP)
	localEndpoint := fmt.Sprintf("%s://%s:2379", protocol, nodeIP)

	// Skip expensive endpoint health checks on first create — etcd
	// hasn't been configured yet so connections always fail, wasting
	// ~40 seconds on retries.  If a config file exists, etcd was
	// previously set up and we need to check membership/health for
	// rejoin decisions.
	lbOK := false
	localOK := false
	isMember := false
	_, configErr := os.Stat("/etc/etcd/etcd.conf.yaml")
	if configErr == nil {
		lbOK = etcdHealthy(executor, lbEndpoint, certDir, cfg.Shared.TLSDisabled)
		localOK = etcdHealthy(executor, localEndpoint, certDir, cfg.Shared.TLSDisabled)
		if lbOK {
			isMember = checkMembership(executor, lbEndpoint, certDir, cfg.Shared.TLSDisabled, cfg.Shared.InstanceName, nodeIP)
		}
	}
	discoveryOK := checkDiscoveryURL(executor, cfg.Master.EtcdDiscoveryURL)

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
		cs, err := writeAndStartEtcd(executor, etcdConf)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)

	default:
		return moduleapi.Result{}, fmt.Errorf("etcd: no valid discovery URL or healthy LB endpoint")
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"etcdTag": cfg.Master.EtcdTag},
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
		containerImage = "quay.io/coreos/"
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
`, containerImage, cfg.Master.EtcdTag)

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
	etcdVersion := strings.TrimPrefix(cfg.Master.EtcdTag, "v")
	if etcdVersion == "" {
		return nil, nil
	}

	etcdDir := "/srv/magnum/etcd"
	change, err := executor.EnsureDir(etcdDir, 0o755)
	if err != nil {
		return nil, err
	}
	var changes []host.Change
	if change != nil {
		changes = append(changes, *change)
	}

	tgzURL := fmt.Sprintf("https://github.com/etcd-io/etcd/releases/download/v%s/etcd-v%s-linux-amd64.tar.gz",
		etcdVersion, etcdVersion)
	tgzPath := fmt.Sprintf("%s/etcd-v%s-linux-amd64.tar.gz", etcdDir, etcdVersion)

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
	return writeAndStartEtcd(executor, conf)
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
	return writeAndStartEtcd(executor, conf)
}

func cleanupEtcd(executor *host.Executor) {
	_ = executor.Run("systemctl", "stop", "etcd")
	_ = executor.Run("podman", "rm", "-f", "etcd")
	// Match bash behavior: wait for etcd to fully stop before proceeding.
	if executor.Apply {
		time.Sleep(5 * time.Second)
	}
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

func writeAndStartEtcd(executor *host.Executor, config string) ([]host.Change, error) {
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

	// Wait for etcd to be functionally healthy after start.  In a
	// multi-master setup the discovery process and quorum election can
	// take significant time.  Without this wait, downstream phases
	// (kube-apiserver, controller-manager) fail because etcd isn't ready.
	//
	// Use the HTTP loopback listener (http://127.0.0.1:2379) which is
	// always configured without TLS, avoiding cert mismatch issues
	// during initial cluster formation.
	if started && executor.Apply {
		healthy := false
		for i := 0; i < 60; i++ {
			args := []string{"ETCDCTL_API=3", "/usr/local/bin/etcdctl",
				"--endpoints=http://127.0.0.1:2379", "--command-timeout=5s",
				"endpoint", "health"}
			_, err := executor.RunCapture("env", args...)
			if err == nil {
				healthy = true
				break
			}
			time.Sleep(5 * time.Second)
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
	args := []string{"--endpoints=" + endpoint, "--command-timeout=5s"}
	if !tlsDisabled {
		args = append(args,
			"--cacert="+certDir+"/ca.crt",
			"--cert="+certDir+"/server.crt",
			"--key="+certDir+"/server.key",
		)
	}
	return args
}

// runEtcdctl runs etcdctl with ETCDCTL_API=3 and retry logic (3 attempts, 3s delay),
// matching bash's run_etcdctl function.
func runEtcdctl(executor *host.Executor, args ...string) (string, error) {
	fullArgs := append([]string{"ETCDCTL_API=3", "/usr/local/bin/etcdctl"}, args...)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		out, err := executor.RunCapture("env", fullArgs...)
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
		outputs["etcdTag"] = pulumi.String(cfg.Master.EtcdTag)
		outputs["etcdDiscoveryUrl"] = pulumi.String(cfg.Master.EtcdDiscoveryURL)
	}
	if err := ctx.RegisterResourceOutputs(res, outputs); err != nil {
		return nil, err
	}
	return res, nil
}

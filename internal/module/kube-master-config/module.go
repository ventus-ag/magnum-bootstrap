package kubemasterconfig

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/kubeletconfig"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "kube-master-config" }
func (Module) Dependencies() []string {
	return []string{"master-certificates", "cert-api-manager", "etcd", "client-tools", "container-runtime"}
}

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	// Network driver setup.
	cs, err := setupNetwork(cfg, executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, cs...)

	// Sysctl.
	cs, err = setupSysctl(executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, cs...)

	// Service files.
	cs, err = writeServiceFiles(cfg, executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, cs...)

	// Kubernetes config files.
	cs, err = writeKubeConfigs(cfg, executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, cs...)

	// Kubelet config.
	cs, err = writeKubeletConfig(cfg, executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, cs...)

	// Docker sysconfig (non-containerd runtimes only).
	cs, err = configureDockerSysconfig(cfg, executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, cs...)

	// Signal service restarts for any changes detected.
	if len(changes) > 0 && req.Restarts != nil {
		for _, svc := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "kubelet", "kube-proxy"} {
			req.Restarts.Add(svc, "kube-master-config changed")
		}
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{
			"role":    "master",
			"kubeTag": cfg.Shared.KubeTag,
		},
	}, nil
}

func setupNetwork(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change

	switch cfg.Shared.NetworkDriver {
	case "flannel":
		for _, dir := range []string{"/opt/cni/bin", "/srv/magnum/kubernetes/cni"} {
			change, err := executor.EnsureDir(dir, 0o755)
			if err != nil {
				return nil, err
			}
			if change != nil {
				changes = append(changes, *change)
			}
		}

		{
			cniTag := "v1.6.2"
			cniURL := fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/%s/cni-plugins-linux-amd64-%s.tgz",
				cniTag, cniTag)
			cniTgz := fmt.Sprintf("/srv/magnum/kubernetes/cni/cni-plugins-linux-amd64-%s.tgz", cniTag)

			dl, err := executor.DownloadFileWithRetry(context.Background(), cniURL, cniTgz, 0o644, 5)
			if err != nil {
				return nil, fmt.Errorf("download CNI plugins: %w", err)
			}
			if dl.Change != nil {
				changes = append(changes, *dl.Change)
			}
			if dl.Changed {
				_ = executor.Run("tar", "-C", "/opt/cni/bin", "-xzf", cniTgz)
				_ = executor.Run("chmod", "+x", "/opt/cni/bin/.")
			}
		}

		// Kernel modules for flannel.
		_ = executor.Run("modprobe", "-a", "vxlan", "br_netfilter")
		change, err := executor.EnsureFile("/etc/modules-load.d/flannel.conf", []byte("vxlan\nbr_netfilter\n"), 0o644)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}

	case "calico":
		// Calico requires rp_filter set to 1 for strict reverse path filtering.
		change, err := executor.EnsureLine("/etc/sysctl.conf", "net.ipv4.conf.all.rp_filter = 1", 0o644)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
			_ = executor.Run("sysctl", "-p")
		}

		// If NetworkManager is active, tell it to ignore calico/tunl interfaces.
		nmActive, _ := executor.RunCapture("systemctl", "is-active", "NetworkManager.service")
		if strings.TrimSpace(nmActive) == "active" {
			calicoNMConf := "[keyfile]\nunmanaged-devices=interface-name:cali*;interface-name:tunl*\n"
			change, err := executor.EnsureFile("/etc/NetworkManager/conf.d/calico.conf", []byte(calicoNMConf), 0o644)
			if err != nil {
				return nil, err
			}
			if change != nil {
				changes = append(changes, *change)
				_ = executor.Run("systemctl", "restart", "NetworkManager")
			}
		}
	}

	return changes, nil
}

func setupSysctl(executor *host.Executor) ([]host.Change, error) {
	content := `net.ipv4.conf.default.rp_filter=2
net.ipv4.conf.*.rp_filter=2
net.ipv4.conf.all.promote_secondaries = 1
net.ipv4.conf.*.accept_source_route = 1
net.ipv4.ip_unprivileged_port_start = 0
net.ipv4.ping_group_range = 0 2147483647
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward = 1
fs.inotify.max_user_instances = 8192
fs.inotify.max_user_watches = 1048576
`
	// br_netfilter must be loaded for bridge-nf-call-iptables sysctl to work.
	_ = executor.Run("modprobe", "br_netfilter")
	modChange, modErr := executor.EnsureFile("/etc/modules-load.d/k8s-bridge.conf", []byte("br_netfilter\n"), 0o644)
	if modErr != nil {
		return nil, modErr
	}

	change, err := executor.EnsureFile("/etc/sysctl.d/k8s_custom.conf", []byte(content), 0o644)
	if err != nil {
		return nil, err
	}
	var changes []host.Change
	if modChange != nil {
		changes = append(changes, *modChange)
	}
	if change != nil {
		changes = append(changes, *change)
	}
	if len(changes) > 0 {
		_ = executor.Run("sysctl", "--system")
	}
	return changes, nil
}

func writeServiceFiles(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	if !cfg.Shared.UsePodman {
		return nil, nil
	}

	var changes []host.Change
	services := map[string]string{
		"kube-apiserver":          apiServerService(cfg),
		"kube-controller-manager": controllerManagerService(cfg),
		"kube-scheduler":          schedulerService(cfg),
		"kubelet":                 kubeletService(),
		"kube-proxy":              kubeProxyService(cfg),
	}

	for name, content := range services {
		path := fmt.Sprintf("/etc/systemd/system/%s.service", name)
		change, err := executor.EnsureFile(path, []byte(content), 0o644)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	if len(changes) > 0 {
		_ = executor.Run("systemctl", "daemon-reload")
	}

	return changes, nil
}

func writeKubeConfigs(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change
	certDir := "/etc/kubernetes/certs"
	apiPort := cfg.Shared.KubeAPIPort
	if apiPort == 0 {
		apiPort = 6443
	}

	// Proxy kubeconfig.
	proxyKC := buildKubeconfig("kube-proxy", certDir+"/proxy.crt", certDir+"/proxy.key",
		certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))
	change, err := executor.EnsureFile("/etc/kubernetes/proxy-kubeconfig.yaml", []byte(proxyKC), 0o640)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Controller kubeconfig.
	controllerKC := buildKubeconfig("controller", certDir+"/controller.crt", certDir+"/controller.key",
		certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))
	change, err = executor.EnsureFile("/etc/kubernetes/controller-kubeconfig.yaml", []byte(controllerKC), 0o640)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Scheduler kubeconfig.
	schedulerKC := buildKubeconfig("scheduler", certDir+"/scheduler.crt", certDir+"/scheduler.key",
		certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))
	change, err = executor.EnsureFile("/etc/kubernetes/scheduler-kubeconfig.yaml", []byte(schedulerKC), 0o640)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Kubelet kubeconfig.
	kubeletKC := buildKubeconfig(fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName),
		certDir+"/kubelet.crt", certDir+"/kubelet.key",
		certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))
	change, err = executor.EnsureFile("/etc/kubernetes/kubelet.conf", []byte(kubeletKC), 0o640)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Keystone webhook config (must be written before API server env
	// so the file exists when the API server starts).
	if cfg.Shared.KeystoneAuthEnabled {
		webhookKC := buildKeystoneWebhookConfig(cfg, certDir, apiPort)
		change, err = executor.EnsureFile("/etc/kubernetes/keystone_webhook_config.yaml", []byte(webhookKC), 0o640)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	// API server env.
	apiServerArgs := buildAPIServerArgs(cfg)
	apiServerEnv := fmt.Sprintf(`KUBE_API_ADDRESS="--bind-address=0.0.0.0 --secure-port=%d"
KUBE_SERVICE_ADDRESSES="--service-cluster-ip-range=%s"
KUBE_API_ARGS="%s"
KUBE_ETCD_SERVERS="--etcd-servers=http://127.0.0.1:2379,http://127.0.0.1:4001"
`, apiPort, cfg.Shared.PortalNetworkCIDR, apiServerArgs)
	change, err = executor.EnsureFile("/etc/kubernetes/apiserver", []byte(apiServerEnv), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Controller manager env.
	controllerArgs := buildControllerManagerArgs(cfg)
	change, err = executor.EnsureFile("/etc/kubernetes/controller-manager",
		[]byte(fmt.Sprintf("KUBE_CONTROLLER_MANAGER_ARGS=\"%s\"\n", controllerArgs)), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Scheduler env.
	schedulerArgs := fmt.Sprintf("--leader-elect=true --kubeconfig=/etc/kubernetes/scheduler-kubeconfig.yaml")
	change, err = executor.EnsureFile("/etc/kubernetes/scheduler",
		[]byte(fmt.Sprintf("KUBE_SCHEDULER_ARGS=\"%s\"\n", schedulerArgs)), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Proxy env.
	proxyArgs := fmt.Sprintf("--kubeconfig=/etc/kubernetes/proxy-kubeconfig.yaml --cluster-cidr=%s --hostname-override=%s %s",
		cfg.Shared.PodsNetworkCIDR, cfg.Shared.InstanceName, cfg.Shared.KubeProxyOptions)
	change, err = executor.EnsureFile("/etc/kubernetes/proxy",
		[]byte(fmt.Sprintf("KUBE_PROXY_ARGS=\"%s\"\n", strings.TrimSpace(proxyArgs))), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Base config.
	change, err = executor.EnsureFile("/etc/kubernetes/config", []byte("KUBE_LOG_LEVEL=\"--v=2\"\n"), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// PodSecurity admission defaults to enforce: "privileged" (allow all).
	// No explicit config needed — the built-in defaults are permissive.
	// Namespace labels (applied in cluster-rbac) provide additional safety.

	return changes, nil
}

func writeKubeletConfig(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change
	certDir := "/etc/kubernetes/certs"

	// Create directories.
	for _, dir := range []string{"/etc/kubernetes/manifests", "/srv/magnum/kubernetes"} {
		change, err := executor.EnsureDir(dir, 0o755)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	nodeIP := cfg.ResolveNodeIP()
	cgroupDriver := cfg.ResolveCgroupDriver()
	dnsServiceIP := cfg.Shared.DNSServiceIP
	dnsClusterDomain := cfg.Shared.DNSClusterDomain
	if dnsClusterDomain == "" {
		dnsClusterDomain = "cluster.local"
	}

	// Get instance ID for providerID.  Always fetch (even in preview)
	// so the generated content matches what is on disk — otherwise
	// preview shows a false diff every time.
	instanceID := ""
	out, err := executor.RunCapture("curl", "-s", "--max-time", "5", "http://169.254.169.254/openstack/latest/meta_data.json")
	if err == nil {
		if idx := strings.Index(out, `"uuid"`); idx >= 0 {
			rest := out[idx+7:]
			if start := strings.Index(rest, `"`); start >= 0 {
				rest = rest[start+1:]
				if end := strings.Index(rest, `"`); end >= 0 {
					instanceID = rest[:end]
				}
			}
		}
	}

	// Taint master nodes so workloads don't schedule on them.
	// K8s < 1.25 used "master", K8s >= 1.25 uses "control-plane".
	registerWithTaints := ""
	if kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 25) {
		registerWithTaints = `registerWithTaints:
  - effect: "NoSchedule"
    key: "node-role.kubernetes.io/control-plane"
`
	} else {
		registerWithTaints = `registerWithTaints:
  - effect: "NoSchedule"
    key: "node-role.kubernetes.io/master"
`
	}
	featureGates := kubeletconfig.FeatureGatesYAML(cfg.Shared.KubeTag)

	kubeletConfig := fmt.Sprintf(`---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
authentication:
  anonymous:
    enabled: false
  webhook:
    cacheTTL: 0s
    enabled: true
  x509:
    clientCAFile: "%s/ca.crt"
authorization:
  mode: Webhook
  webhook:
    cacheAuthorizedTTL: 0s
    cacheUnauthorizedTTL: 0s
cgroupDriver: %s
clusterDNS:
- %s
clusterDomain: %s
address: %s
failSwapOn: true
port: 10250
readOnlyPort: 0
containerLogMaxFiles: 5
containerLogMaxSize: 10Mi
%smaxPods: 110
podPidsLimit: -1
providerID: openstack:///%s
resolvConf: /run/systemd/resolve/resolv.conf
volumePluginDir: /var/lib/kubelet/volumeplugins
rotateCertificates: true
tlsCertFile: %s/kubelet.crt
tlsPrivateKeyFile: %s/kubelet.key
staticPodPath: /etc/kubernetes/manifests
runtimeRequestTimeout: 15m
eventRecordQPS: 5
containerRuntimeEndpoint: unix:///run/containerd/containerd.sock
%s
`, certDir, cgroupDriver, dnsServiceIP, dnsClusterDomain, nodeIP,
		registerWithTaints, instanceID, certDir, certDir, featureGates)

	change, err := executor.EnsureFile("/etc/kubernetes/kubelet-config.yaml", []byte(kubeletConfig), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Kubelet args.
	kubeletArgs := buildKubeletArgs(cfg)
	kubeletEnv := fmt.Sprintf(`KUBELET_ADDRESS="--node-ip=%s"
KUBELET_HOSTNAME="--hostname-override=%s"
KUBELET_ARGS="%s"
`, nodeIP, cfg.Shared.InstanceName, kubeletArgs)
	change, err = executor.EnsureFile("/etc/kubernetes/kubelet.env", []byte(kubeletEnv), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	return changes, nil
}

// configureDockerSysconfig adjusts /etc/sysconfig/docker for non-containerd
// runtimes: sets json-file log driver and adds insecure registry if configured.
// This is a no-op when containerd is the runtime or the file does not exist.
func configureDockerSysconfig(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	if cfg.Shared.ContainerRuntime == "containerd" {
		return nil, nil
	}
	const dockerSysconfigPath = "/etc/sysconfig/docker"
	data, err := os.ReadFile(dockerSysconfigPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dockerSysconfigPath, err)
	}

	content := string(data)

	// Remove --log-driver=journald so json-file takes effect.
	content = strings.ReplaceAll(content, "--log-driver=journald", "")

	// Ensure json-file log driver with rotation options is present in OPTIONS.
	logOpts := "--log-driver=json-file --log-opt max-size=10m --log-opt max-file=5 "
	if !strings.Contains(content, "--log-driver=json-file") {
		// Insert log options right after OPTIONS=" or OPTIONS='
		re := regexp.MustCompile(`(?m)^OPTIONS=(['"])`)
		content = re.ReplaceAllString(content, "OPTIONS=${1}"+logOpts)
	}

	// Add insecure registry if configured and not already present.
	if cfg.Shared.InsecureRegistryURL != "" {
		insecureLine := fmt.Sprintf("INSECURE_REGISTRY='--insecure-registry %s'", cfg.Shared.InsecureRegistryURL)
		if !strings.Contains(content, "INSECURE_REGISTRY=") {
			content = strings.TrimRight(content, "\n") + "\n" + insecureLine + "\n"
		}
	}

	change, err := executor.EnsureFile(dockerSysconfigPath, []byte(content), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		return []host.Change{*change}, nil
	}
	return nil, nil
}

func buildKubeletArgs(cfg config.Config) string {
	args := []string{
		"--kubeconfig /etc/kubernetes/kubelet.conf",
		"--config=/etc/kubernetes/kubelet-config.yaml",
		fmt.Sprintf("--node-labels=magnum.openstack.org/role=%s", cfg.Shared.NodegroupRole),
		fmt.Sprintf("--node-labels=magnum.openstack.org/nodegroup=%s", cfg.Shared.NodegroupName),
	}
	if cfg.Shared.ContainerRuntime == "containerd" {
		args = append(args, "--runtime-cgroups=/system.slice/containerd.service")
	}
	if cfg.Shared.KubeletOptions != "" {
		args = append(args, cfg.Shared.KubeletOptions)
	}
	return strings.Join(args, " ")
}

func buildAPIServerArgs(cfg config.Config) string {
	certDir := "/etc/kubernetes/certs"

	authzMode := "Node,RBAC"
	if cfg.Shared.KeystoneAuthEnabled {
		authzMode = "Node,Webhook,RBAC"
	}

	args := []string{
		"--runtime-config=api/all=true",
		"--kubelet-preferred-address-types=InternalIP,Hostname,ExternalIP",
	}
	// --allow-privileged was documented as "removed" in K8s 1.27, but the
	// default changed in K8s 1.34+ to disallow privileged containers.
	// Always pass it explicitly to ensure infrastructure DaemonSets
	// (cinder-csi, etc.) that need privileged: true are accepted.
	allowPriv := cfg.Shared.KubeAllowPriv
	if allowPriv == "" {
		allowPriv = "true"
	}
	args = append(args, "--allow-privileged="+allowPriv)
	args = append(args,
		fmt.Sprintf("--authorization-mode=%s", authzMode),
		fmt.Sprintf("--tls-cert-file=%s/server.crt", certDir),
		fmt.Sprintf("--tls-private-key-file=%s/server.key", certDir),
		fmt.Sprintf("--client-ca-file=%s/ca.crt", certDir),
		fmt.Sprintf("--service-account-key-file=%s/service_account.key", certDir),
		fmt.Sprintf("--service-account-signing-key-file=%s/service_account_private.key", certDir),
		"--service-account-issuer=https://kubernetes.default.svc.cluster.local",
		fmt.Sprintf("--kubelet-certificate-authority=%s/ca.crt", certDir),
		fmt.Sprintf("--kubelet-client-certificate=%s/server.crt", certDir),
		fmt.Sprintf("--kubelet-client-key=%s/server.key", certDir),
		fmt.Sprintf("--proxy-client-cert-file=%s/server.crt", certDir),
		fmt.Sprintf("--proxy-client-key-file=%s/server.key", certDir),
		fmt.Sprintf("--requestheader-client-ca-file=%s/ca.crt", certDir),
		"--requestheader-allowed-names=front-proxy-client,kube,kubernetes",
		"--requestheader-extra-headers-prefix=X-Remote-Extra-",
		"--requestheader-group-headers=X-Remote-Group",
		"--requestheader-username-headers=X-Remote-User",
	)
	if cfg.Shared.KeystoneAuthEnabled {
		args = append(args,
			"--authentication-token-webhook-config-file=/etc/kubernetes/keystone_webhook_config.yaml",
			"--authorization-webhook-config-file=/etc/kubernetes/keystone_webhook_config.yaml",
		)
	}
	if cfg.Shared.KubeAPIOptions != "" {
		args = append(args, cfg.Shared.KubeAPIOptions)
	}
	return strings.Join(args, " ")
}

func buildControllerManagerArgs(cfg config.Config) string {
	certDir := "/etc/kubernetes/certs"
	args := []string{
		"--leader-elect=true",
		fmt.Sprintf("--cluster-name=%s", cfg.Shared.ClusterUUID),
		"--allocate-node-cidrs=true",
		"--kubeconfig=/etc/kubernetes/controller-kubeconfig.yaml",
		fmt.Sprintf("--cluster-cidr=%s", cfg.Shared.PodsNetworkCIDR),
		fmt.Sprintf("--service-account-private-key-file=%s/service_account_private.key", certDir),
		fmt.Sprintf("--root-ca-file=%s/ca.crt", certDir),
		"--use-service-account-credentials=true",
	}
	// --secure-port=0 disabled the secure serving port. K8s >= 1.26 deprecated
	// this pattern; keep it only for older versions.
	if !kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 26) {
		args = append(args, "--secure-port=0")
	}
	if cfg.Shared.CloudProviderEnabled {
		args = append(args, "--cloud-provider=external")
	}
	if cfg.Shared.CertManagerAPI {
		args = append(args, fmt.Sprintf("--cluster-signing-cert-file=%s/ca.crt", certDir))
		args = append(args, fmt.Sprintf("--cluster-signing-key-file=%s/ca.key", certDir))
	}
	if cfg.Shared.KubeControllerOptions != "" {
		args = append(args, cfg.Shared.KubeControllerOptions)
	}
	return strings.Join(args, " ")
}

func buildKubeconfig(user, certFile, keyFile, caFile, server string) string {
	return fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
    certificate-authority: %s
    server: %s
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: %s
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: %s
  user:
    as-user-extra: {}
    client-certificate: %s
    client-key: %s
`, caFile, server, user, user, certFile, keyFile)
}

func buildKeystoneWebhookConfig(cfg config.Config, certDir string, apiPort int) string {
	return fmt.Sprintf(`---
apiVersion: v1
clusters:
- cluster:
    certificate-authority: %s/ca.crt
    server: https://127.0.0.1:%d
  name: %s
contexts:
- context:
    cluster: %s
    user: admin
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: admin
  user:
    as-user-extra: {}
    client-certificate: %s/admin.crt
    client-key: %s/admin.key
`, certDir, apiPort, cfg.Shared.ClusterUUID, cfg.Shared.ClusterUUID, certDir, certDir)
}

func apiServerService(cfg config.Config) string {
	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/"
	}
	return fmt.Sprintf(`[Unit]
Description=kube-apiserver
After=network.target
[Service]
EnvironmentFile=/etc/sysconfig/heat-params
EnvironmentFile=/etc/kubernetes/config
EnvironmentFile=/etc/kubernetes/apiserver
ExecStartPre=/bin/mkdir -p /etc/kubernetes/
ExecStartPre=-/usr/bin/podman rm kube-apiserver
ExecStart=/bin/bash -c '/usr/bin/podman run --name kube-apiserver \
    --net host \
    --volume /etc/kubernetes:/etc/kubernetes:ro,z \
    --volume /usr/lib/os-release:/etc/os-release:ro \
    --volume /etc/ssl/certs:/etc/ssl/certs:ro \
    --volume /run:/run \
    --volume /etc/pki/tls/certs:/usr/share/ca-certificates:ro \
    %skube-apiserver-%s:%s \
    kube-apiserver \
    $KUBE_LOG_LEVEL $KUBE_ETCD_SERVERS $KUBE_API_ADDRESS $KUBE_SERVICE_ADDRESSES $KUBE_API_ARGS'
ExecStop=-/usr/bin/podman stop kube-apiserver
Delegate=yes
KillMode=process
Restart=always
RestartSec=10
TimeoutStartSec=10min
[Install]
WantedBy=multi-user.target
`, prefix, cfg.Shared.Arch, cfg.Shared.KubeTag)
}

func controllerManagerService(cfg config.Config) string {
	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/"
	}
	return fmt.Sprintf(`[Unit]
Description=kube-controller-manager
After=network.target kube-apiserver.service
[Service]
EnvironmentFile=/etc/sysconfig/heat-params
EnvironmentFile=/etc/kubernetes/config
EnvironmentFile=/etc/kubernetes/controller-manager
ExecStartPre=/bin/mkdir -p /etc/kubernetes/
ExecStartPre=-/usr/bin/podman rm kube-controller-manager
ExecStart=/bin/bash -c '/usr/bin/podman run --name kube-controller-manager \
    --net host \
    --volume /etc/kubernetes:/etc/kubernetes:ro,z \
    --volume /usr/lib/os-release:/etc/os-release:ro \
    --volume /etc/ssl/certs:/etc/ssl/certs:ro \
    --volume /run:/run \
    --volume /etc/pki/tls/certs:/usr/share/ca-certificates:ro \
    %skube-controller-manager-%s:%s \
    kube-controller-manager \
    $KUBE_LOG_LEVEL $KUBE_MASTER $KUBE_CONTROLLER_MANAGER_ARGS'
ExecStop=-/usr/bin/podman stop kube-controller-manager
Delegate=yes
KillMode=process
Restart=always
RestartSec=10
TimeoutStartSec=10min
[Install]
WantedBy=multi-user.target
`, prefix, cfg.Shared.Arch, cfg.Shared.KubeTag)
}

func schedulerService(cfg config.Config) string {
	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/"
	}
	return fmt.Sprintf(`[Unit]
Description=kube-scheduler
After=network.target kube-apiserver.service
[Service]
EnvironmentFile=/etc/sysconfig/heat-params
EnvironmentFile=/etc/kubernetes/config
EnvironmentFile=/etc/kubernetes/scheduler
ExecStartPre=/bin/mkdir -p /etc/kubernetes/
ExecStartPre=-/usr/bin/podman rm kube-scheduler
ExecStart=/bin/bash -c '/usr/bin/podman run --name kube-scheduler \
    --net host \
    --volume /etc/kubernetes:/etc/kubernetes:ro,z \
    --volume /usr/lib/os-release:/etc/os-release:ro \
    --volume /etc/ssl/certs:/etc/ssl/certs:ro \
    --volume /run:/run \
    --volume /etc/pki/tls/certs:/usr/share/ca-certificates:ro \
    %skube-scheduler-%s:%s \
    kube-scheduler \
    $KUBE_LOG_LEVEL $KUBE_MASTER $KUBE_SCHEDULER_ARGS'
ExecStop=-/usr/bin/podman stop kube-scheduler
Delegate=yes
KillMode=process
Restart=always
RestartSec=10
TimeoutStartSec=10min
[Install]
WantedBy=multi-user.target
`, prefix, cfg.Shared.Arch, cfg.Shared.KubeTag)
}

func kubeletService() string {
	return `[Unit]
Description=Kubelet
After=network.target containerd.service
Wants=containerd.service
[Service]
EnvironmentFile=/etc/sysconfig/heat-params
EnvironmentFile=/etc/kubernetes/config
EnvironmentFile=-/etc/kubernetes/kubelet.env
ExecStartPre=/bin/mkdir -p /etc/kubernetes/cni/net.d
ExecStartPre=/bin/mkdir -p /etc/kubernetes/manifests
ExecStartPre=/bin/mkdir -p /var/lib/calico
ExecStartPre=/bin/mkdir -p /var/lib/containerd
ExecStartPre=/bin/mkdir -p /var/lib/docker
ExecStartPre=/bin/mkdir -p /var/lib/kubelet/volumeplugins
ExecStartPre=/bin/mkdir -p /opt/cni/bin
ExecStart=/usr/local/bin/kubelet \
    $KUBE_LOG_LEVEL $KUBE_LOGTOSTDERR $KUBELET_API_SERVER $KUBELET_ADDRESS $KUBELET_HOSTNAME $KUBELET_ARGS
Delegate=yes
KillMode=process
Restart=always
RestartSec=10
TimeoutStartSec=10min
[Install]
WantedBy=multi-user.target
`
}

func kubeProxyService(cfg config.Config) string {
	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/"
	}
	return fmt.Sprintf(`[Unit]
Description=kube-proxy
After=network.target
[Service]
EnvironmentFile=/etc/sysconfig/heat-params
EnvironmentFile=/etc/kubernetes/config
EnvironmentFile=/etc/kubernetes/proxy
ExecStartPre=/bin/mkdir -p /etc/kubernetes/
ExecStartPre=-/usr/bin/podman rm kube-proxy
ExecStart=/bin/bash -c '/usr/bin/podman run --name kube-proxy \
    --privileged \
    --net host \
    --volume /etc/kubernetes:/etc/kubernetes:ro,z \
    --volume /usr/lib/os-release:/etc/os-release:ro \
    --volume /etc/ssl/certs:/etc/ssl/certs:ro \
    --volume /run:/run \
    --volume /sys/fs/cgroup:/sys/fs/cgroup \
    --volume /lib/modules:/lib/modules:ro \
    --volume /etc/pki/tls/certs:/usr/share/ca-certificates:ro \
    %skube-proxy-%s:%s \
    kube-proxy \
    $KUBE_LOG_LEVEL $KUBE_MASTER $KUBE_PROXY_ARGS'
ExecStop=-/usr/bin/podman stop kube-proxy
Delegate=yes
KillMode=process
Restart=always
RestartSec=10
TimeoutStartSec=10min
[Install]
WantedBy=multi-user.target
`, prefix, cfg.Shared.Arch, cfg.Shared.KubeTag)
}

// Destroy removes master kubernetes configuration files and CNI binaries.
func (Module) Destroy(_ context.Context, _ config.Config, req moduleapi.Request) error {
	if req.Logger != nil {
		req.Logger.Infof("kube-master-config destroy: removing config files and CNI binaries")
	}
	_ = os.Remove("/etc/sysctl.d/k8s_custom.conf")
	_ = os.Remove("/etc/modules-load.d/flannel.conf")
	_ = os.Remove("/etc/kubernetes/proxy-kubeconfig.yaml")
	_ = os.Remove("/etc/kubernetes/controller-kubeconfig.yaml")
	_ = os.Remove("/etc/kubernetes/scheduler-kubeconfig.yaml")
	_ = os.Remove("/etc/kubernetes/kubelet.conf")
	_ = os.Remove("/etc/kubernetes/kubelet-config.yaml")
	_ = os.Remove("/etc/kubernetes/kubelet.env")
	_ = os.Remove("/etc/kubernetes/config")
	_ = os.Remove("/etc/kubernetes/apiserver")
	_ = os.Remove("/etc/kubernetes/controller-manager")
	_ = os.Remove("/etc/kubernetes/scheduler")
	_ = os.Remove("/etc/kubernetes/proxy")
	_ = os.RemoveAll("/opt/cni/bin")

	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:KubeMasterConfig", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"kubeTag":       pulumi.String(cfg.Shared.KubeTag),
		"networkDriver": pulumi.String(cfg.Shared.NetworkDriver),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

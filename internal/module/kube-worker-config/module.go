package kubeworkerconfig

import (
	"context"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "kube-worker-config" }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	// Network setup.
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

	// Kubeconfig and config files.
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

	// Docker sysconfig.
	cs, err = configureDockerSysconfig(cfg, executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, cs...)

	// Signal service restarts for any changes detected.
	if len(changes) > 0 && req.Restarts != nil {
		for _, svc := range []string{"kubelet", "kube-proxy"} {
			req.Restarts.Add(svc, "kube-worker-config changed")
		}
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{
			"role":    "worker",
			"kubeTag": cfg.Shared.KubeTag,
		},
	}, nil
}

func setupNetwork(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change

	if cfg.Shared.NetworkDriver == "flannel" {
		for _, dir := range []string{"/opt/cni/bin", "/srv/magnum/kubernetes/cni"} {
			change, err := executor.EnsureDir(dir, 0o755)
			if err != nil {
				return nil, err
			}
			if change != nil {
				changes = append(changes, *change)
			}
		}

		if cfg.Shared.FlannelCNITag != "" {
			cniURL := fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/%s/cni-plugins-linux-amd64-%s.tgz",
				cfg.Shared.FlannelCNITag, cfg.Shared.FlannelCNITag)
			cniTgz := fmt.Sprintf("/srv/magnum/kubernetes/cni/cni-plugins-linux-amd64-%s.tgz", cfg.Shared.FlannelCNITag)

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
	}

	if cfg.Shared.NetworkDriver == "calico" {
		// Calico requires rp_filter=1 and higher max_map_count.
		sysctlContent := `net.ipv4.conf.all.rp_filter = 1
vm.max_map_count = 262144
`
		change, err := executor.EnsureFile("/etc/sysctl.d/calico.conf", []byte(sysctlContent), 0o644)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
			_ = executor.Run("sysctl", "-p", "/etc/sysctl.d/calico.conf")
		}

		// If NetworkManager is active, tell it to ignore calico/tunl interfaces.
		nmActive, _ := executor.RunCapture("systemctl", "is-active", "NetworkManager")
		if strings.TrimSpace(nmActive) == "active" {
			nmConf := `[keyfile]
unmanaged-devices=interface-name:cali*;interface-name:tunl*
`
			change, err = executor.EnsureFile("/etc/NetworkManager/conf.d/calico.conf", []byte(nmConf), 0o644)
			if err != nil {
				return nil, err
			}
			if change != nil {
				changes = append(changes, *change)
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
`
	change, err := executor.EnsureFile("/etc/sysctl.d/k8s_custom.conf", []byte(content), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		_ = executor.Run("sysctl", "--system")
		return []host.Change{*change}, nil
	}
	return nil, nil
}

func writeServiceFiles(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	if !cfg.Shared.UsePodman {
		return nil, nil
	}

	var changes []host.Change
	services := map[string]string{
		"kubelet":    kubeletService(),
		"kube-proxy": kubeProxyService(cfg),
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

	masterIP := ""
	if cfg.Worker != nil {
		masterIP = cfg.Worker.KubeMasterIP
	}
	apiPort := cfg.Shared.KubeAPIPort
	if apiPort == 0 {
		apiPort = 6443
	}
	kubeMasterURI := fmt.Sprintf("https://%s:%d", masterIP, apiPort)

	// Etcd server IP: fall back to master IP if not set.
	etcdServerIP := ""
	if cfg.Worker != nil {
		etcdServerIP = cfg.Worker.EtcdServerIP
		if etcdServerIP == "" {
			etcdServerIP = cfg.Worker.KubeMasterIP
		}
	}

	// Base config.
	baseConfig := fmt.Sprintf(`KUBE_LOG_LEVEL="--v=3"
KUBE_ALLOW_PRIV="--allow-privileged=%s"
KUBE_ETCD_SERVERS="--etcd-servers=http://%s:2379"
KUBE_MASTER="--master=%s"
`, cfg.Shared.KubeAllowPriv, etcdServerIP, kubeMasterURI)
	change, err := executor.EnsureFile("/etc/kubernetes/config", []byte(baseConfig), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Kubelet kubeconfig.
	var kubeletKC string
	if cfg.Shared.TLSDisabled {
		kubeletKC = buildInsecureKubeconfig(
			fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName), kubeMasterURI)
	} else {
		kubeletKC = buildKubeconfig(
			fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName),
			certDir+"/kubelet.crt", certDir+"/kubelet.key",
			certDir+"/ca.crt", kubeMasterURI)
	}
	change, err = executor.EnsureFile("/etc/kubernetes/kubelet.conf", []byte(kubeletKC), 0o640)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Proxy kubeconfig.
	var proxyKC string
	if cfg.Shared.TLSDisabled {
		proxyKC = buildInsecureKubeconfig("kube-proxy", kubeMasterURI)
	} else {
		proxyKC = buildKubeconfig("kube-proxy",
			certDir+"/proxy.crt", certDir+"/proxy.key",
			certDir+"/ca.crt", kubeMasterURI)
	}
	change, err = executor.EnsureFile("/etc/kubernetes/proxy-config.yaml", []byte(proxyKC), 0o640)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Proxy env.
	proxyArgs := fmt.Sprintf("--kubeconfig=/etc/kubernetes/proxy-config.yaml --cluster-cidr=%s --hostname-override=%s %s",
		cfg.Shared.PodsNetworkCIDR, cfg.Shared.InstanceName, cfg.Shared.KubeProxyOptions)
	change, err = executor.EnsureFile("/etc/kubernetes/proxy",
		[]byte(fmt.Sprintf("KUBE_PROXY_ARGS=\"%s\"\n", strings.TrimSpace(proxyArgs))), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Environment.
	change, err = executor.EnsureLine("/etc/environment",
		fmt.Sprintf("KUBERNETES_MASTER=%s", kubeMasterURI), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	return changes, nil
}

func writeKubeletConfig(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change
	certDir := "/etc/kubernetes/certs"

	change, err := executor.EnsureDir("/etc/kubernetes/manifests", 0o755)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	nodeIP := cfg.ResolveNodeIP()
	cgroupDriver := cfg.Shared.CgroupDriver
	if cgroupDriver == "" {
		cgroupDriver = "systemd"
	}
	dnsServiceIP := cfg.Shared.DNSServiceIP
	dnsClusterDomain := cfg.Shared.DNSClusterDomain
	if dnsClusterDomain == "" {
		dnsClusterDomain = "cluster.local"
	}

	instanceID := ""
	if executor.Apply {
		out, err := executor.RunCapture("curl", "-s", "http://169.254.169.254/openstack/latest/meta_data.json")
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
	}

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
failSwapOn: True
port: 10250
readOnlyPort: 0
containerLogMaxFiles: 5
containerLogMaxSize: 10Mi
maxPods: 110
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
featureGates:
  GracefulNodeShutdown: false
`, certDir, cgroupDriver, dnsServiceIP, dnsClusterDomain, nodeIP,
		instanceID, certDir, certDir)

	change, err = executor.EnsureFile("/etc/kubernetes/kubelet-config.yaml", []byte(kubeletConfig), 0o644)
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

func buildInsecureKubeconfig(user, server string) string {
	return fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
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
`, server, user, user)
}

func configureDockerSysconfig(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	if cfg.Shared.ContainerRuntime != "docker" {
		return nil, nil
	}

	// Only configure if /etc/sysconfig/docker exists.
	_, err := executor.RunCapture("test", "-f", "/etc/sysconfig/docker")
	if err != nil {
		return nil, nil
	}

	var changes []host.Change

	// Set json-file log driver.
	content := `OPTIONS="--log-driver=json-file --log-opt max-size=10m --log-opt max-file=5"
`
	if cfg.Shared.InsecureRegistryURL != "" {
		content += fmt.Sprintf("INSECURE_REGISTRY=\"--insecure-registry %s\"\n", cfg.Shared.InsecureRegistryURL)
	}

	change, err := executor.EnsureFile("/etc/sysconfig/docker", []byte(content), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	return changes, nil
}

func kubeletService() string {
	return `[Unit]
Description=Kubelet
Wants=rpc-statd.service

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
    $KUBE_LOG_LEVEL $KUBELET_API_SERVER $KUBELET_ADDRESS $KUBELET_PORT $KUBELET_HOSTNAME $KUBELET_ARGS
Delegate=yes
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
Description=kube-proxy via registry.k8s.io/kube-proxy
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
Restart=always
RestartSec=10
TimeoutStartSec=10min
[Install]
WantedBy=multi-user.target
`, prefix, cfg.Shared.Arch, cfg.Shared.KubeTag)
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:KubeWorkerConfig", name, res, opts...); err != nil {
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

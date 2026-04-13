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
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/kubeletconfig"
	"github.com/ventus-ag/magnum-bootstrap/internal/module/kubecommon"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "kube-master-config" }
func (Module) Dependencies() []string {
	return []string{"master-certificates", "cert-api-manager", "kube-os-config", "client-tools", "container-runtime", "stop-services"}
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
	cs, err = kubecommon.SetupKubernetesSysctl(executor)
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
		cs, err := kubecommon.SetupFlannelCNI(executor)
		if err != nil {
			return nil, err
		}
		changes = append(changes, cs...)

	case "calico":
		// Calico requires rp_filter set to 1 for strict reverse path filtering.
		lineResult, err := (hostresource.LineSpec{Path: "/etc/sysctl.conf", Line: "net.ipv4.conf.all.rp_filter = 1", Mode: 0o644}).Apply(executor)
		if err != nil {
			return nil, err
		}
		if lineResult.Changed {
			changes = append(changes, lineResult.Changes...)
			_ = executor.Run("sysctl", "-p")
		}

		// If NetworkManager is active, tell it to ignore calico/tunl interfaces.
		nmActive, _ := executor.RunCapture("systemctl", "is-active", "NetworkManager.service")
		if strings.TrimSpace(nmActive) == "active" {
			calicoNMConf := "[keyfile]\nunmanaged-devices=interface-name:cali*;interface-name:tunl*\n"
			fileResult, err := (hostresource.FileSpec{Path: "/etc/NetworkManager/conf.d/calico.conf", Content: []byte(calicoNMConf), Mode: 0o644}).Apply(executor)
			if err != nil {
				return nil, err
			}
			if fileResult.Changed {
				changes = append(changes, fileResult.Changes...)
				serviceResult, err := (hostresource.SystemdServiceSpec{Unit: "NetworkManager.service", SkipIfMissing: true, Restart: true, RestartReason: "calico NetworkManager config changed"}).Apply(executor)
				if err != nil {
					return nil, err
				}
				changes = append(changes, serviceResult.Changes...)
			}
		}
	}

	return changes, nil
}

func writeKubeletConfig(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change

	// Create directories.
	for _, dir := range []string{"/etc/kubernetes/manifests", "/srv/magnum/kubernetes"} {
		result, err := (hostresource.DirectorySpec{Path: dir, Mode: 0o755}).Apply(executor)
		if err != nil {
			return nil, err
		}
		changes = append(changes, result.Changes...)
	}

	dnsClusterDomain := cfg.Shared.DNSClusterDomain
	if dnsClusterDomain == "" {
		dnsClusterDomain = "cluster.local"
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

	opts := kubecommon.KubeletConfigOpts{
		CertDir:            "/etc/kubernetes/certs",
		CgroupDriver:       cfg.ResolveCgroupDriver(),
		DNSServiceIP:       cfg.Shared.DNSServiceIP,
		DNSClusterDomain:   dnsClusterDomain,
		NodeIP:             cfg.ResolveNodeIP(),
		InstanceID:         kubecommon.FetchInstanceID(executor),
		FeatureGates:       kubeletconfig.FeatureGatesYAML(cfg.Shared.KubeTag),
		RegisterWithTaints: registerWithTaints,
	}

	change, err := applyFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/kubelet-config.yaml", Content: []byte(kubecommon.RenderKubeletConfig(opts)), Mode: 0o644})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	// Kubelet args.
	nodeIP := opts.NodeIP
	kubeletArgs := kubecommon.BuildKubeletArgs(cfg)
	kubeletEnv := fmt.Sprintf(`KUBELET_ADDRESS="--node-ip=%s"
KUBELET_HOSTNAME="--hostname-override=%s"
KUBELET_ARGS="%s"
`, nodeIP, cfg.Shared.InstanceName, kubeletArgs)
	change, err = applyFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/kubelet.env", Content: []byte(kubeletEnv), Mode: 0o644})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
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

	result, err := (hostresource.FileSpec{Path: dockerSysconfigPath, Content: []byte(content), Mode: 0o644}).Apply(executor)
	if err != nil {
		return nil, err
	}
	if result.Changed {
		return result.Changes, nil
	}
	return nil, nil
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
	childOpts := append(opts, pulumi.Parent(res))
	if err := registerServiceFileResources(ctx, name+"-service-files", cfg, childOpts...); err != nil {
		return nil, err
	}
	if err := registerKubeConfigResources(ctx, name+"-kubeconfigs", cfg, childOpts...); err != nil {
		return nil, err
	}
	if err := registerKubeletConfigResources(ctx, name+"-kubelet", cfg, childOpts...); err != nil {
		return nil, err
	}
	if err := registerNetworkResources(ctx, name+"-network", cfg, childOpts...); err != nil {
		return nil, err
	}
	if err := registerDockerSysconfigResource(ctx, name+"-docker-sysconfig", cfg, childOpts...); err != nil {
		return nil, err
	}
	if err := kubecommon.RegisterKubernetesSysctl(ctx, name+"-k8s-sysctl", childOpts...); err != nil {
		return nil, err
	}
	if cfg.Shared.NetworkDriver == "flannel" {
		if err := kubecommon.RegisterFlannelCNI(ctx, name+"-flannel-cni", childOpts...); err != nil {
			return nil, err
		}
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"kubeTag":       pulumi.String(cfg.Shared.KubeTag),
		"networkDriver": pulumi.String(cfg.Shared.NetworkDriver),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

func registerKubeletConfigResources(ctx *pulumi.Context, name string, cfg config.Config, opts ...pulumi.ResourceOption) error {
	for _, dir := range []string{"/etc/kubernetes/manifests", "/srv/magnum/kubernetes"} {
		if _, err := hostsdk.RegisterDirectorySpec(ctx, name+"-dir-"+strings.ReplaceAll(strings.Trim(dir, "/"), "/", "-"), hostresource.DirectorySpec{Path: dir, Mode: 0o755}, opts...); err != nil {
			return err
		}
	}
	executor := host.NewExecutor(false, nil)
	dnsClusterDomain := cfg.Shared.DNSClusterDomain
	if dnsClusterDomain == "" {
		dnsClusterDomain = "cluster.local"
	}
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
	optsCfg := kubecommon.KubeletConfigOpts{
		CertDir:            "/etc/kubernetes/certs",
		CgroupDriver:       cfg.ResolveCgroupDriver(),
		DNSServiceIP:       cfg.Shared.DNSServiceIP,
		DNSClusterDomain:   dnsClusterDomain,
		NodeIP:             cfg.ResolveNodeIP(),
		InstanceID:         kubecommon.FetchInstanceID(executor),
		FeatureGates:       kubeletconfig.FeatureGatesYAML(cfg.Shared.KubeTag),
		RegisterWithTaints: registerWithTaints,
	}
	if _, err := hostsdk.RegisterFileSpec(ctx, name+"-config", hostresource.FileSpec{Path: "/etc/kubernetes/kubelet-config.yaml", Content: []byte(kubecommon.RenderKubeletConfig(optsCfg)), Mode: 0o644}, opts...); err != nil {
		return err
	}
	kubeletEnv := fmt.Sprintf(`KUBELET_ADDRESS="--node-ip=%s"
KUBELET_HOSTNAME="--hostname-override=%s"
KUBELET_ARGS="%s"
`, optsCfg.NodeIP, cfg.Shared.InstanceName, kubecommon.BuildKubeletArgs(cfg))
	_, err := hostsdk.RegisterFileSpec(ctx, name+"-env", hostresource.FileSpec{Path: "/etc/kubernetes/kubelet.env", Content: []byte(kubeletEnv), Mode: 0o644}, opts...)
	return err
}

func registerNetworkResources(ctx *pulumi.Context, name string, cfg config.Config, opts ...pulumi.ResourceOption) error {
	if cfg.Shared.NetworkDriver != "calico" {
		return nil
	}
	if _, err := hostsdk.RegisterLineSpec(ctx, name+"-sysctl-line", hostresource.LineSpec{Path: "/etc/sysctl.conf", Line: "net.ipv4.conf.all.rp_filter = 1", Mode: 0o644}, opts...); err != nil {
		return err
	}
	executor := host.NewExecutor(false, nil)
	nmActive, _ := executor.RunCapture("systemctl", "is-active", "NetworkManager.service")
	if strings.TrimSpace(nmActive) != "active" {
		return nil
	}
	_, err := hostsdk.RegisterFileSpec(ctx, name+"-nm-config", hostresource.FileSpec{Path: "/etc/NetworkManager/conf.d/calico.conf", Content: []byte("[keyfile]\nunmanaged-devices=interface-name:cali*;interface-name:tunl*\n"), Mode: 0o644}, opts...)
	return err
}

func registerDockerSysconfigResource(ctx *pulumi.Context, name string, cfg config.Config, opts ...pulumi.ResourceOption) error {
	if cfg.Shared.ContainerRuntime == "containerd" {
		return nil
	}
	content, err := renderDockerSysconfigContent(cfg)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = hostsdk.RegisterFileSpec(ctx, name, hostresource.FileSpec{Path: "/etc/sysconfig/docker", Content: []byte(content), Mode: 0o644}, opts...)
	return err
}

func renderDockerSysconfigContent(cfg config.Config) (string, error) {
	const dockerSysconfigPath = "/etc/sysconfig/docker"
	data, err := os.ReadFile(dockerSysconfigPath)
	if err != nil {
		return "", err
	}

	content := string(data)
	content = strings.ReplaceAll(content, "--log-driver=journald", "")
	logOpts := "--log-driver=json-file --log-opt max-size=10m --log-opt max-file=5 "
	if !strings.Contains(content, "--log-driver=json-file") {
		re := regexp.MustCompile(`(?m)^OPTIONS=(['"])`)
		content = re.ReplaceAllString(content, "OPTIONS=${1}"+logOpts)
	}
	if cfg.Shared.InsecureRegistryURL != "" {
		insecureLine := fmt.Sprintf("INSECURE_REGISTRY='--insecure-registry %s'", cfg.Shared.InsecureRegistryURL)
		if !strings.Contains(content, "INSECURE_REGISTRY=") {
			content = strings.TrimRight(content, "\n") + "\n" + insecureLine + "\n"
		}
	}
	return content, nil
}

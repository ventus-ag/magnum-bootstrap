package kubeworkerconfig

import (
	"context"
	"fmt"
	"os"
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

func (Module) PhaseID() string { return "kube-worker-config" }
func (Module) Dependencies() []string {
	return []string{"worker-certificates", "kube-os-config", "client-tools", "container-runtime", "stop-services"}
}

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
		cs, err := kubecommon.SetupFlannelCNI(executor)
		if err != nil {
			return nil, err
		}
		changes = append(changes, cs...)
	}

	if cfg.Shared.NetworkDriver == "calico" {
		// Calico requires rp_filter=1 and higher max_map_count.
		sysctlContent := `net.ipv4.conf.all.rp_filter = 1
vm.max_map_count = 262144
`
		sysctlResult, err := (hostresource.SysctlSpec{Path: "/etc/sysctl.d/calico.conf", Content: []byte(sysctlContent), Mode: 0o644, ReloadArg: []string{"-p", "/etc/sysctl.d/calico.conf"}}).Apply(executor)
		if err != nil {
			return nil, err
		}
		changes = append(changes, sysctlResult.Changes...)

		// If NetworkManager is active, tell it to ignore calico/tunl interfaces.
		nmActive, _ := executor.RunCapture("systemctl", "is-active", "NetworkManager")
		if strings.TrimSpace(nmActive) == "active" {
			nmConf := `[keyfile]
unmanaged-devices=interface-name:cali*;interface-name:tunl*
`
			fileResult, err := (hostresource.FileSpec{Path: "/etc/NetworkManager/conf.d/calico.conf", Content: []byte(nmConf), Mode: 0o644}).Apply(executor)
			if err != nil {
				return nil, err
			}
			changes = append(changes, fileResult.Changes...)
		}
	}

	return changes, nil
}

func writeKubeletConfig(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change

	change, err := (hostresource.DirectorySpec{Path: "/etc/kubernetes/manifests", Mode: 0o755}).Apply(executor)
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	dnsClusterDomain := cfg.Shared.DNSClusterDomain
	if dnsClusterDomain == "" {
		dnsClusterDomain = "cluster.local"
	}

	opts := kubecommon.KubeletConfigOpts{
		CertDir:          "/etc/kubernetes/certs",
		CgroupDriver:     cfg.ResolveCgroupDriver(),
		DNSServiceIP:     cfg.Shared.DNSServiceIP,
		DNSClusterDomain: dnsClusterDomain,
		NodeIP:           cfg.ResolveNodeIP(),
		InstanceID:       kubecommon.FetchInstanceID(executor),
		FeatureGates:     kubeletconfig.FeatureGatesYAML(cfg.Shared.KubeTag),
	}

	change, err = applyWorkerFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/kubelet-config.yaml", Content: []byte(kubecommon.RenderKubeletConfig(opts)), Mode: 0o644})
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
	change, err = applyWorkerFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/kubelet.env", Content: []byte(kubeletEnv), Mode: 0o644})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	return changes, nil
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

	change, err := applyWorkerFileResource(executor, hostresource.FileSpec{Path: "/etc/sysconfig/docker", Content: []byte(content), Mode: 0o644})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	return changes, nil
}

// Destroy removes worker kubernetes configuration files and CNI binaries.
func (Module) Destroy(_ context.Context, _ config.Config, req moduleapi.Request) error {
	if req.Logger != nil {
		req.Logger.Infof("kube-worker-config destroy: removing config files and CNI binaries")
	}
	_ = os.Remove("/etc/sysctl.d/k8s_custom.conf")
	_ = os.Remove("/etc/modules-load.d/flannel.conf")
	_ = os.Remove("/etc/kubernetes/proxy-kubeconfig.yaml")
	_ = os.Remove("/etc/kubernetes/kubelet.conf")
	_ = os.Remove("/etc/kubernetes/kubelet-config.yaml")
	_ = os.Remove("/etc/kubernetes/kubelet.env")
	_ = os.Remove("/etc/kubernetes/config")
	_ = os.Remove("/etc/kubernetes/proxy")
	_ = os.RemoveAll("/opt/cni/bin")

	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:KubeWorkerConfig", name, res, opts...); err != nil {
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
	if _, err := hostsdk.RegisterDirectorySpec(ctx, name+"-dir-manifests", hostresource.DirectorySpec{Path: "/etc/kubernetes/manifests", Mode: 0o755}, opts...); err != nil {
		return err
	}
	executor := host.NewExecutor(false, nil)
	dnsClusterDomain := cfg.Shared.DNSClusterDomain
	if dnsClusterDomain == "" {
		dnsClusterDomain = "cluster.local"
	}
	optsCfg := kubecommon.KubeletConfigOpts{
		CertDir:          "/etc/kubernetes/certs",
		CgroupDriver:     cfg.ResolveCgroupDriver(),
		DNSServiceIP:     cfg.Shared.DNSServiceIP,
		DNSClusterDomain: dnsClusterDomain,
		NodeIP:           cfg.ResolveNodeIP(),
		InstanceID:       kubecommon.FetchInstanceID(executor),
		FeatureGates:     kubeletconfig.FeatureGatesYAML(cfg.Shared.KubeTag),
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
	if _, err := hostsdk.RegisterSysctlSpec(ctx, name+"-calico-sysctl", hostresource.SysctlSpec{Path: "/etc/sysctl.d/calico.conf", Content: []byte("net.ipv4.conf.all.rp_filter = 1\nvm.max_map_count = 262144\n"), Mode: 0o644, ReloadArg: []string{"-p", "/etc/sysctl.d/calico.conf"}}, opts...); err != nil {
		return err
	}
	executor := host.NewExecutor(false, nil)
	nmActive, _ := executor.RunCapture("systemctl", "is-active", "NetworkManager")
	if strings.TrimSpace(nmActive) != "active" {
		return nil
	}
	_, err := hostsdk.RegisterFileSpec(ctx, name+"-nm-config", hostresource.FileSpec{Path: "/etc/NetworkManager/conf.d/calico.conf", Content: []byte("[keyfile]\nunmanaged-devices=interface-name:cali*;interface-name:tunl*\n"), Mode: 0o644}, opts...)
	return err
}

func registerDockerSysconfigResource(ctx *pulumi.Context, name string, cfg config.Config, opts ...pulumi.ResourceOption) error {
	if cfg.Shared.ContainerRuntime != "docker" {
		return nil
	}
	if _, err := os.Stat("/etc/sysconfig/docker"); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	content := `OPTIONS="--log-driver=json-file --log-opt max-size=10m --log-opt max-file=5"
`
	if cfg.Shared.InsecureRegistryURL != "" {
		content += fmt.Sprintf("INSECURE_REGISTRY=\"--insecure-registry %s\"\n", cfg.Shared.InsecureRegistryURL)
	}
	_, err := hostsdk.RegisterFileSpec(ctx, name, hostresource.FileSpec{Path: "/etc/sysconfig/docker", Content: []byte(content), Mode: 0o644}, opts...)
	return err
}

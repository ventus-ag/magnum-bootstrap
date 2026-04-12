package kubeworkerconfig

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/kubeletconfig"
	"github.com/ventus-ag/magnum-bootstrap/internal/module/kubecommon"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
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

func writeKubeletConfig(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change

	change, err := executor.EnsureDir("/etc/kubernetes/manifests", 0o755)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
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

	change, err = executor.EnsureFile("/etc/kubernetes/kubelet-config.yaml", []byte(kubecommon.RenderKubeletConfig(opts)), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Kubelet args.
	nodeIP := opts.NodeIP
	kubeletArgs := kubecommon.BuildKubeletArgs(cfg)
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
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"kubeTag":       pulumi.String(cfg.Shared.KubeTag),
		"networkDriver": pulumi.String(cfg.Shared.NetworkDriver),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

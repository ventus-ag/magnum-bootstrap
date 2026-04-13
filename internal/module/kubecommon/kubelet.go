package kubecommon

import (
	"fmt"
	"strings"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/kubeletconfig"
)

// KubeletConfigOpts contains the parameters for rendering kubelet-config.yaml.
type KubeletConfigOpts struct {
	CertDir            string
	CgroupDriver       string
	DNSServiceIP       string
	DNSClusterDomain   string
	NodeIP             string
	InstanceID         string
	FeatureGates       string
	RegisterWithTaints string // empty for workers, taint block for masters
}

// RenderKubeletConfig produces the kubelet-config.yaml content.
func RenderKubeletConfig(opts KubeletConfigOpts) string {
	return fmt.Sprintf(`---
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
`, opts.CertDir, opts.CgroupDriver, opts.DNSServiceIP, opts.DNSClusterDomain,
		opts.NodeIP, opts.RegisterWithTaints, opts.InstanceID,
		opts.CertDir, opts.CertDir, opts.FeatureGates)
}

// BuildKubeletArgs produces the KUBELET_ARGS value for kubelet.env.
func BuildKubeletArgs(cfg config.Config) string {
	nodeLabels := []string{
		fmt.Sprintf("magnum.openstack.org/role=%s", cfg.Shared.NodegroupRole),
	}
	if cfg.Shared.NodegroupName != "" {
		nodeLabels = append(nodeLabels, fmt.Sprintf("magnum.openstack.org/nodegroup=%s", cfg.Shared.NodegroupName))
	}
	args := []string{
		"--kubeconfig /etc/kubernetes/kubelet.conf",
		"--config=/etc/kubernetes/kubelet-config.yaml",
		"--node-labels=" + strings.Join(nodeLabels, ","),
	}
	if cfg.Shared.ContainerRuntime == "containerd" {
		args = append(args, "--runtime-cgroups=/system.slice/containerd.service")
	}
	// containerRuntimeEndpoint was added to KubeletConfiguration in K8s 1.27.
	// For older versions the config field is silently ignored, so pass it as
	// a CLI flag to tell kubelet where the CRI socket is.
	if !kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 27) {
		endpoint := "unix:///run/containerd/containerd.sock"
		if cfg.Shared.ContainerRuntime == "docker" {
			endpoint = "unix:///var/run/cri-dockerd.sock"
		}
		args = append(args, "--container-runtime-endpoint="+endpoint)
		// K8s < 1.24 defaults to --container-runtime=docker (built-in dockershim).
		// When using containerd, we must explicitly select the remote CRI path.
		if !kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 24) && cfg.Shared.ContainerRuntime == "containerd" {
			args = append(args, "--container-runtime=remote")
		}
	}
	if cfg.Shared.KubeletOptions != "" {
		args = append(args, cfg.Shared.KubeletOptions)
	}
	return strings.Join(args, " ")
}

// FetchInstanceID retrieves the OpenStack instance UUID from the metadata
// service. Returns an empty string on any failure. Always fetches even in
// dry-run so preview content matches what is on disk.
func FetchInstanceID(executor *host.Executor) string {
	out, err := executor.RunCapture("curl", "-s", "--max-time", "5", "http://169.254.169.254/openstack/latest/meta_data.json")
	if err != nil {
		return ""
	}
	if idx := strings.Index(out, `"uuid"`); idx >= 0 {
		rest := out[idx+7:]
		if start := strings.Index(rest, `"`); start >= 0 {
			rest = rest[start+1:]
			if end := strings.Index(rest, `"`); end >= 0 {
				return rest[:end]
			}
		}
	}
	return ""
}

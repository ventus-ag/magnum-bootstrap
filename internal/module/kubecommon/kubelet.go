package kubecommon

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/kubeletconfig"
)

// systemdResolvConf is the systemd-resolved stub used by Fedora CoreOS and
// default Ubuntu cloud images; fallbackResolvConf is the plain libc resolver.
const (
	systemdResolvConf  = "/run/systemd/resolve/resolv.conf"
	fallbackResolvConf = "/etc/resolv.conf"
)

// NodeResolvConf returns the kubelet resolvConf path for this node: the
// systemd-resolved stub when present (FCoS + default Ubuntu 22.04), otherwise
// /etc/resolv.conf. Called from both Run() and Register() on the same node so
// the rendered config is identical (no spurious drift).
func NodeResolvConf() string {
	if _, err := os.Stat(systemdResolvConf); err == nil {
		return systemdResolvConf
	}
	return fallbackResolvConf
}

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
	ResolvConf         string // empty defaults to the systemd-resolved stub
}

// RenderKubeletConfig produces the kubelet-config.yaml content.
func RenderKubeletConfig(opts KubeletConfigOpts) string {
	resolvConf := opts.ResolvConf
	if resolvConf == "" {
		resolvConf = systemdResolvConf
	}
	// providerID is IMMUTABLE once the node registers. Baking a broken
	// "openstack:///" (empty instance ID after a metadata hiccup) can never
	// be fixed without recreating the Node object and breaks OCCM/Cinder/
	// autoscaler node mapping. Omit the field instead — the cloud controller
	// resolves the instance by node name when providerID is unset.
	providerIDLine := ""
	if opts.InstanceID != "" {
		providerIDLine = "providerID: openstack:///" + opts.InstanceID + "\n"
	}
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
%sresolvConf: %s
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
		opts.NodeIP, opts.RegisterWithTaints, providerIDLine, resolvConf,
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
// service (with retries), falling back to the providerID already recorded in
// the on-disk kubelet config so a transient metadata outage neither bakes a
// broken providerID nor flaps the rendered file (and with it a kubelet
// restart) between runs. Always fetches even in dry-run so preview content
// matches what is on disk.
func FetchInstanceID(executor *host.Executor) string {
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		out, err := executor.RunCapture("curl", "-s", "--max-time", "5", "http://169.254.169.254/openstack/latest/meta_data.json")
		if err != nil {
			continue
		}
		if id := parseMetadataUUID(out); id != "" {
			return id
		}
	}
	return existingProviderInstanceID("/etc/kubernetes/kubelet-config.yaml")
}

func parseMetadataUUID(out string) string {
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

var providerIDRe = regexp.MustCompile(`providerID:\s*openstack:///([0-9a-fA-F-]+)`)

func existingProviderInstanceID(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	m := providerIDRe.FindSubmatch(data)
	if m == nil {
		return ""
	}
	return string(m[1])
}

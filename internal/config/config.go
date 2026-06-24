package config

import (
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ventus-ag/magnum-bootstrap/internal/buildinfo"
)

type Role string

const (
	RoleUnknown Role = "unknown"
	RoleMaster  Role = "master"
	RoleWorker  Role = "worker"
)

func (r Role) String() string {
	return string(r)
}

type Operation string

const (
	OperationCreate   Operation = "create"
	OperationCARotate Operation = "ca-rotate"
)

func (o Operation) String() string {
	return string(o)
}

type Config struct {
	InputChecksum string            `json:"inputChecksum"`
	Raw           map[string]string `json:"raw,omitempty"`
	Shared        SharedConfig      `json:"shared"`
	Master        *MasterConfig     `json:"master,omitempty"`
	Worker        *WorkerConfig     `json:"worker,omitempty"`
	Trigger       TriggerConfig     `json:"trigger"`

	// Distro is the node OS family (the os-release ID, e.g. "fedora-coreos",
	// "ubuntu"), detected at Load() time from /etc/os-release — NOT a heat-param.
	// The reconciler is one binary for both Fedora CoreOS and Ubuntu; modules
	// branch on IsFCoS()/IsUbuntu() for the handful of OS-specific paths.
	Distro string `json:"distro,omitempty"`
}

// IsUbuntu reports whether the node runs Ubuntu (or a Debian derivative).
func (c Config) IsUbuntu() bool {
	d := strings.ToLower(c.Distro)
	return strings.Contains(d, "ubuntu") || strings.Contains(d, "debian")
}

// IsFCoS reports whether the node runs Fedora CoreOS (or another RHEL-family
// immutable image). It is the DEFAULT when the distro is unknown, so existing
// Fedora CoreOS behavior is preserved if detection ever fails.
func (c Config) IsFCoS() bool {
	if c.IsUbuntu() {
		return false
	}
	return true
}

// detectDistro reads the os-release ID from the node. Returns "" when neither
// file is present (e.g. unit tests on a dev box), which IsFCoS() treats as FCoS.
func detectDistro() string {
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		if b, err := os.ReadFile(p); err == nil {
			if id := parseOSReleaseID(string(b)); id != "" {
				return id
			}
		}
	}
	return ""
}

// parseOSReleaseID extracts the lowercased ID= value from os-release content.
func parseOSReleaseID(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "ID="); ok {
			return strings.ToLower(strings.Trim(strings.TrimSpace(v), "\"'"))
		}
	}
	return ""
}

type SharedConfig struct {
	InstanceName              string `json:"instanceName"`
	NodegroupRole             string `json:"nodegroupRole"`
	NodegroupName             string `json:"nodegroupName"`
	Arch                      string `json:"arch"`
	TLSDisabled               bool   `json:"tlsDisabled"`
	KubeTag                   string `json:"kubeTag"`
	KubeVersion               string `json:"kubeVersion"`
	KubeAPIPort               int    `json:"kubeApiPort"`
	ClusterUUID               string `json:"clusterUuid"`
	NetworkDriver             string `json:"networkDriver"`
	ContainerRuntime          string `json:"containerRuntime"`
	SELinuxMode               string `json:"selinuxMode"`
	ReconcilerVersion         string `json:"reconcilerVersion"`
	ReconcilerBinaryURL       string `json:"reconcilerBinaryUrl"`
	ReconcilerBinaryURLSHA256 string `json:"reconcilerBinaryUrlSha256"`
	AuthURL                   string `json:"authUrl"`
	TrusteeUserID             string `json:"trusteeUserId"`
	TrusteePassword           string `json:"trusteePassword"`
	TrustID                   string `json:"trustId"`
	OctaviaEnabled            bool   `json:"octaviaEnabled"`
	ClusterSubnet             string `json:"clusterSubnet"`
	ExternalNetworkID         string `json:"externalNetworkId"`
	ClusterNetworkName        string `json:"clusterNetworkName"`
	HTTPProxy                 string `json:"httpProxy"`
	HTTPSProxy                string `json:"httpsProxy"`
	NoProxy                   string `json:"noProxy"`

	// Container runtime
	CgroupDriver         string `json:"cgroupDriver"`
	UsePodman            bool   `json:"usePodman"`
	ContainerInfraPrefix string `json:"containerInfraPrefix"`

	// Node identity
	KubeNodeIP       string `json:"kubeNodeIp"`
	KubeNodePublicIP string `json:"kubeNodePublicIp"`

	// Network
	PortalNetworkCIDR string `json:"portalNetworkCidr"`
	PodsNetworkCIDR   string `json:"podsNetworkCidr"`

	// API server
	KubeAllowPriv        string `json:"kubeAllowPriv"`
	AdmissionControlList string `json:"admissionControlList"`
	KeystoneAuthEnabled  bool   `json:"keystoneAuthEnabled"`
	CloudProviderEnabled bool   `json:"cloudProviderEnabled"`

	// Certificates
	CertManagerAPI               bool   `json:"certManagerApi"`
	CAKey                        string `json:"caKey"`
	KubeServiceAccountKey        string `json:"kubeServiceAccountKey"`
	KubeServiceAccountPrivateKey string `json:"kubeServiceAccountPrivateKey"`
	VerifyCA                     bool   `json:"verifyCA"`
	MagnumURL                    string `json:"magnumUrl"`

	// DNS
	DNSServiceIP     string `json:"dnsServiceIp"`
	DNSClusterDomain string `json:"dnsClusterDomain"`

	// Storage
	DockerVolume        string `json:"dockerVolume"`
	DockerVolumeSize    int    `json:"dockerVolumeSize"`
	DockerStorageDriver string `json:"dockerStorageDriver"`
	EnableCinder        bool   `json:"enableCinder"`

	// Docker
	InsecureRegistryURL string `json:"insecureRegistryUrl"`

	// Component options
	KubeletOptions        string `json:"kubeletOptions"`
	KubeAPIOptions        string `json:"kubeApiOptions"`
	KubeControllerOptions string `json:"kubeControllerOptions"`
	KubeProxyOptions      string `json:"kubeProxyOptions"`

	// Node roles
	LeadNodeRoleName string `json:"leadNodeRoleName"`
	KubeImageDigest  string `json:"kubeImageDigest"`

	// Cluster addons (master-0 only, after API ready)
	RegionName string `json:"regionName"`

	// Flannel
	FlannelBackend     string `json:"flannelBackend"`
	FlannelNetworkCIDR string `json:"flannelNetworkCidr"`

	// Kubernetes Dashboard
	KubeDashboardEnabled bool `json:"kubeDashboardEnabled"`

	// Metrics server
	MetricsServerEnabled bool `json:"metricsServerEnabled"`

	// Auto-healing (magnum-auto-healer)
	AutoHealingEnabled    bool   `json:"autoHealingEnabled"`
	AutoHealingController string `json:"autoHealingController"`

	// Auto-scaling
	AutoScalingEnabled bool `json:"autoScalingEnabled"`
	MinNodeCount       int  `json:"minNodeCount"`
	MaxNodeCount       int  `json:"maxNodeCount"`

	// Volume / CSI
	VolumeDriver     string `json:"volumeDriver"`
	CinderCSIEnabled bool   `json:"cinderCsiEnabled"`
	ManilaCSIEnabled bool   `json:"manilaCSIEnabled"`

	// NVIDIA GPU Operator (master-0 only, requires GPU nodes)
	GPUOperatorEnabled bool `json:"gpuOperatorEnabled"`

	// OS auto-upgrade (Zincati on Fedora CoreOS)
	OSAutoUpgradeEnabled bool `json:"osAutoUpgradeEnabled"`

	// Post-install
	PostInstallManifestURL string `json:"postInstallManifestUrl"`
}

type MasterConfig struct {
	NumberOfMasters       int    `json:"numberOfMasters"`
	KubeAPIPublicAddress  string `json:"kubeApiPublicAddress"`
	KubeAPIPrivateAddress string `json:"kubeApiPrivateAddress"`
	// InitialCluster is the static etcd initial-cluster member list
	// ("name0=https://ip0:2380,name1=https://ip1:2380,...") used to bootstrap a
	// new cluster. When empty, a first/single master bootstraps a one-node
	// cluster from its own peer URL (which then grows via member-add as other
	// masters join through the etcd LB).
	InitialCluster string `json:"initialCluster"`
	MasterHostname string `json:"masterHostname"`
	EtcdLBVIP      string `json:"etcdLbVip"`
	EtcdVolume     string `json:"etcdVolume"`
	EtcdVolumeSize int    `json:"etcdVolumeSize"`
}

type WorkerConfig struct {
	KubeMasterIP      string `json:"kubeMasterIp"`
	EtcdServerIP      string `json:"etcdServerIp"`
	RegistryEnabled   bool   `json:"registryEnabled"`
	RegistryPort      int    `json:"registryPort"`
	RegistryContainer string `json:"registryContainer"`
	RegistryInsecure  bool   `json:"registryInsecure"`
	RegistryChunksize int    `json:"registryChunksize"`
	SwiftRegion       string `json:"swiftRegion"`
	TrusteeUsername   string `json:"trusteeUsername"`
	TrusteeDomainID   string `json:"trusteeDomainId"`
}

type TriggerConfig struct {
	CARotationID string `json:"caRotationId"`

	// AppliedCARotationID is set from the previous reconciler state.
	// When it matches CARotationID, the rotation was already applied
	// and the operation falls back to normal create/reconcile.
	AppliedCARotationID string `json:"-"`
}

func (c Config) Role() Role {
	role := strings.ToLower(strings.TrimSpace(c.Shared.NodegroupRole))
	switch role {
	case "master", "control-plane":
		return RoleMaster
	case "worker", "minion":
		return RoleWorker
	default:
		if c.Master != nil && c.Worker == nil {
			return RoleMaster
		}
		if c.Worker != nil && c.Master == nil {
			return RoleWorker
		}
		return RoleUnknown
	}
}

// Operation classifies the reconcile from desired state alone. Upgrade and
// resize are NOT distinct operations to the reconciler: every module converges
// current host/cluster state to the desired heat-params, so a version bump or a
// node add/remove is just ordinary convergence. Only an active CA rotation
// (token-driven) needs a distinct disruptive code path; everything else is a
// create/reconcile. (The former IS_UPGRADE/IS_RESIZE flags were removed — drain
// intent now comes from the KUBE_TAG delta, see DisruptiveServiceCycleNeeded.)
func (c Config) Operation() Operation {
	if c.Trigger.CARotationID != "" &&
		c.Trigger.CARotationID != c.Trigger.AppliedCARotationID {
		return OperationCARotate
	}
	return OperationCreate
}

func (c Config) StackName() string {
	base := c.Shared.InstanceName
	if base == "" {
		base = c.Role().String()
	}
	if base == "" {
		base = "unknown"
	}
	return "node-" + sanitizeIdentifier(base)
}

// EffectiveReconcilerVersion is the version recorded in state and the Pulumi
// reconcilerVersion tag. The launcher now auto-upgrades to the latest release,
// so RECONCILER_VERSION from heat-params is usually empty; prefer the version
// embedded in this binary at build time (the truthful running version). Fall
// back to the heat-params value for dev/test builds where buildinfo.Version is
// the untagged "dev" sentinel.
func (c Config) EffectiveReconcilerVersion() string {
	if buildinfo.IsTaggedRelease() {
		return buildinfo.Version
	}
	return c.Shared.ReconcilerVersion
}

func (c Config) GenerationToken() string {
	if c.Operation() == OperationCARotate {
		return "ca-rotate:" + c.Trigger.CARotationID
	}
	// Includes version upgrades: the token changes with KUBE_TAG, so state
	// records the generation transition without a dedicated upgrade label.
	return "create:" + c.Shared.KubeTag
}

// IsFirstMaster returns true when this node is master-0 (the first master
// node). Cluster-level addons should only run on master-0 to avoid duplicate
// Helm releases or RBAC conflicts.
func (c Config) IsFirstMaster() bool {
	if c.Role() != RoleMaster {
		return false
	}
	// Instance name heuristic: names ending in "master-0".
	if strings.HasSuffix(c.Shared.InstanceName, "master-0") {
		return true
	}
	// Single-master cluster — this node is the only master.
	if c.Master != nil && c.Master.NumberOfMasters == 1 {
		return true
	}
	return false
}

// ResolveNodeIP returns KubeNodeIP from config, falling back to the OpenStack
// metadata service if the config field is empty.
func (c Config) ResolveNodeIP() string {
	if c.Shared.KubeNodeIP != "" {
		return c.Shared.KubeNodeIP
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://169.254.169.254/latest/meta-data/local-ipv4")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

// ResolveCgroupDriver returns the cgroup driver to use.
//
// If CGROUP_DRIVER is set in heat-params, that value is used (explicit
// override). Otherwise the driver is auto-detected from the host:
//   - cgroup v2 → "systemd" (the only supported driver)
//   - cgroup v1 + systemd init → "systemd"
//   - cgroup v1 + non-systemd init → "cgroupfs"
func (c Config) ResolveCgroupDriver() string {
	if c.Shared.CgroupDriver != "" {
		return c.Shared.CgroupDriver
	}
	// Preserve the driver an EXISTING cluster already runs. The bootstrap
	// heat-params do not carry CGROUP_DRIVER, so without this we would detect
	// "systemd" on every cgroup-v2 node and silently FLIP a live cgroupfs
	// cluster to systemd. That flip makes kubelet and containerd disagree
	// during the reconcile window — runc then rejects every new pod sandbox
	// with `expected cgroupsPath to be of format "slice:prefix:name" for
	// systemd cgroups` — and churns every running pod on the node. cgroup
	// driver is a node-local concern (kubelet and containerd on the SAME node
	// must agree; different nodes may differ), so keeping what the node already
	// uses is correct and non-disruptive. Only a genuinely fresh node (no
	// existing kubelet/containerd config) falls through to the host default.
	if existing := ExistingCgroupDriver(); existing != "" {
		return existing
	}
	return detectCgroupDriver()
}

// ExistingCgroupDriver reads the cgroup driver already configured on the node,
// preferring the kubelet config (authoritative for kubelet's runtime behavior)
// and falling back to the containerd setting. Returns "" when neither exists
// (a fresh node), so a new cluster still gets the detected default. Callers also
// use it to detect a genuine driver FLIP (resolved value != on-disk value) so
// they can restart kubelet alongside containerd.
func ExistingCgroupDriver() string {
	if d := cgroupDriverFromKubeletConfig("/etc/kubernetes/kubelet-config.yaml"); d != "" {
		return d
	}
	return cgroupDriverFromContainerdConfig("/etc/containerd/config.toml")
}

func cgroupDriverFromKubeletConfig(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "cgroupDriver:") {
			continue
		}
		v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "cgroupDriver:")), "\"'")
		if v != "" {
			return v
		}
	}
	return ""
}

func cgroupDriverFromContainerdConfig(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "SystemdCgroup") {
			continue
		}
		if strings.Contains(l, "true") {
			return "systemd"
		}
		if strings.Contains(l, "false") {
			return "cgroupfs"
		}
	}
	return ""
}

func detectCgroupDriver() string {
	// cgroup v2 (unified mode) requires systemd as the cgroup driver.
	// Detection uses the same approach as runc/containerd: statfs on
	// /sys/fs/cgroup and check for the cgroup2 filesystem magic number.
	if IsCgroupV2() {
		return "systemd"
	}
	// cgroup v1 with systemd init: systemd driver is recommended.
	// Detection uses sd_booted() semantics: /run/systemd/system/ exists
	// only when the system was booted with systemd as PID 1.
	if isSystemdBooted() {
		return "systemd"
	}
	return "cgroupfs"
}

// IsCgroupV2 returns true if the system is running cgroup v2 (unified mode).
// Uses the same detection as runc's IsCgroup2UnifiedMode: statfs on
// /sys/fs/cgroup with magic number 0x63677270 (CGROUP2_SUPER_MAGIC).
func IsCgroupV2() bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/sys/fs/cgroup", &st); err != nil {
		return false
	}
	return st.Type == 0x63677270 // CGROUP2_SUPER_MAGIC
}

// isSystemdBooted returns true if the system was booted with systemd.
// Equivalent to sd_booted(3): checks for /run/systemd/system/.
func isSystemdBooted() bool {
	fi, err := os.Stat("/run/systemd/system")
	return err == nil && fi.IsDir()
}

// IsPureCARotation returns true when a CA rotation is active. Modules that defer
// to the rotation module (which handles certs and service restarts itself) skip
// their normal work while it is set. Completed rotations are filtered earlier by
// the AppliedCARotationID check in Operation(), so a stale token does not keep
// this true. (Formerly also required !IS_UPGRADE && !IS_RESIZE; those flags were
// removed in favor of state-driven convergence.)
func (c Config) IsPureCARotation() bool {
	return c.Trigger.CARotationID != ""
}

func sanitizeIdentifier(v string) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9-]+`)
	safe := re.ReplaceAllString(strings.ToLower(v), "-")
	safe = strings.Trim(safe, "-")
	if safe == "" {
		return "unknown"
	}
	return safe
}

func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y":
		return true
	default:
		return false
	}
}

// parseFalse returns true when the value is explicitly "false" / "False" / "0".
// Used for fields where the default is true (e.g. ENABLE_CINDER — Cinder is
// enabled unless explicitly set to "False").
func parseFalse(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "n":
		return true
	default:
		return false
	}
}

func parseInt(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return n
}

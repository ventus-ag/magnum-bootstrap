package config

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	CgroupDriver          string `json:"cgroupDriver"`
	UsePodman             bool   `json:"usePodman"`
	ContainerInfraPrefix  string `json:"containerInfraPrefix"`
	HeatContainerAgentTag string `json:"heatContainerAgentTag"`

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

	// Per-nodegroup Kubernetes node metadata (NODE_LABELS / NODE_TAINTS).
	// Invalid entries are dropped at parse time; the warnings are surfaced by
	// prereq-validation so a bad entry never wedges convergence.
	NodeLabels           map[string]string `json:"nodeLabels,omitempty"`
	NodeTaints           []NodeTaint       `json:"nodeTaints,omitempty"`
	NodeMetadataWarnings []string          `json:"nodeMetadataWarnings,omitempty"`

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
	ManilaShareType  string `json:"manilaShareType"`

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

var (
	metadataNodeIPOnce sync.Once
	metadataNodeIP     string
	// metadataNodeIPURL is a var for tests.
	metadataNodeIPURL = "http://169.254.169.254/latest/meta-data/local-ipv4"
)

// ResolveNodeIP returns KubeNodeIP from config, falling back to the OpenStack
// metadata service if the config field is empty.
//
// The metadata result is fetched once per process and cached: many phases
// (certs, etcd, kubelet config) call this independently, and a transient
// metadata failure mid-run would otherwise give them INCONSISTENT node
// identity — e.g. a cert signed without the node-IP SAN that etcd then
// advertises. One consistent answer (even a consistent failure) beats a
// per-call lottery. The fetch validates the HTTP status and that the body is
// an IP, so an error page can never become the "node IP".
func (c Config) ResolveNodeIP() string {
	if c.Shared.KubeNodeIP != "" {
		return c.Shared.KubeNodeIP
	}
	metadataNodeIPOnce.Do(func() {
		metadataNodeIP = fetchMetadataNodeIP()
	})
	return metadataNodeIP
}

func fetchMetadataNodeIP() string {
	client := &http.Client{Timeout: 5 * time.Second}
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		ip, err := fetchMetadataNodeIPOnce(client)
		if err == nil {
			return ip
		}
	}
	return ""
}

func fetchMetadataNodeIPOnce(client *http.Client) (string, error) {
	resp, err := client.Get(metadataNodeIPURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata service returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("metadata service returned %q, not an IP", ip)
	}
	return ip, nil
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

// IsPureCARotation returns true whenever CA_ROTATION_ID is set. WARNING: it is
// NOT applied-aware — CA_ROTATION_ID lingers in heat-params after a rotation
// finalizes (the rotate fragment never clears it), so this stays true for the
// life of the cluster once any rotation has run. Use it only for the always-true
// guards inside the rotation module itself. Callers that need "a rotation is
// ACTIVE (triggered but not yet applied)" must use Operation() == OperationCARotate,
// which consults AppliedCARotationID and correctly returns to create/reconcile
// once the rotation completes.
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

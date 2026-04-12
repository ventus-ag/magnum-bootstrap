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
	OperationUpgrade  Operation = "upgrade"
	OperationResize   Operation = "resize"
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
}

type SharedConfig struct {
	InstanceName              string `json:"instanceName"`
	NodegroupRole             string `json:"nodegroupRole"`
	NodegroupName             string `json:"nodegroupName"`
	Arch                      string `json:"arch"`
	IsUpgrade                 bool   `json:"isUpgrade"`
	IsResize                  bool   `json:"isResize"`
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
	CgroupDriver string `json:"cgroupDriver"`
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

	// Master index (0 = first master, cluster addons only run on master-0)
	MasterIndex int `json:"masterIndex"`
}

type MasterConfig struct {
	NumberOfMasters       int    `json:"numberOfMasters"`
	KubeAPIPublicAddress  string `json:"kubeApiPublicAddress"`
	KubeAPIPrivateAddress string `json:"kubeApiPrivateAddress"`
	EtcdDiscoveryURL string `json:"etcdDiscoveryUrl"`
	MasterHostname   string `json:"masterHostname"`
	EtcdLBVIP             string `json:"etcdLbVip"`
	EtcdVolume            string `json:"etcdVolume"`
	EtcdVolumeSize        int    `json:"etcdVolumeSize"`
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

func (c Config) Operation() Operation {
	switch {
	case c.Shared.IsUpgrade:
		return OperationUpgrade
	case c.Shared.IsResize:
		return OperationResize
	case c.Trigger.CARotationID != "" &&
		c.Trigger.CARotationID != c.Trigger.AppliedCARotationID:
		return OperationCARotate
	default:
		return OperationCreate
	}
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

func (c Config) GenerationToken() string {
	switch c.Operation() {
	case OperationCARotate:
		return "ca-rotate:" + c.Trigger.CARotationID
	case OperationUpgrade:
		return "upgrade:" + c.Shared.KubeTag
	case OperationResize:
		return "resize:" + c.Shared.KubeTag
	default:
		return "create:" + c.Shared.KubeTag
	}
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
	return detectCgroupDriver()
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

// IsPureCARotation returns true when a CA rotation is active and neither an
// upgrade nor a resize is in progress.  Many modules skip their normal work
// during a pure CA rotation because the rotation module handles certs and
// service restarts itself.
func (c Config) IsPureCARotation() bool {
	return c.Trigger.CARotationID != "" && !c.Shared.IsUpgrade && !c.Shared.IsResize
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

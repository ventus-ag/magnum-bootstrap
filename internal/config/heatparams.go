package config

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func Load(path string) (Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}

	raw, err := parseHeatParams(string(content))
	if err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}

	sum := sha256.Sum256(content)
	cfg := Config{
		InputChecksum: hex.EncodeToString(sum[:]),
		Raw:           raw,
		Shared: SharedConfig{
			InstanceName:              raw["INSTANCE_NAME"],
			NodegroupRole:             raw["NODEGROUP_ROLE"],
			NodegroupName:             raw["NODEGROUP_NAME"],
			Arch:                      raw["ARCH"],
			IsUpgrade:                 parseBool(raw["IS_UPGRADE"]),
			IsResize:                  parseBool(raw["IS_RESIZE"]),
			TLSDisabled:               parseBool(raw["TLS_DISABLED"]),
			KubeTag:                   raw["KUBE_TAG"],
			KubeVersion:               raw["KUBE_VERSION"],
			KubeAPIPort:               parseInt(raw["KUBE_API_PORT"]),
			ClusterUUID:               raw["CLUSTER_UUID"],
			NetworkDriver:             raw["NETWORK_DRIVER"],
			ContainerRuntime:          raw["CONTAINER_RUNTIME"],
			SELinuxMode:               raw["SELINUX_MODE"],
			ReconcilerVersion:         raw["RECONCILER_VERSION"],
			ReconcilerBinaryURL:       raw["RECONCILER_BINARY_URL"],
			ReconcilerBinaryURLSHA256: raw["RECONCILER_BINARY_URL_SHA256"],
			AuthURL:                   raw["AUTH_URL"],
			TrusteeUserID:             raw["TRUSTEE_USER_ID"],
			TrusteePassword:           raw["TRUSTEE_PASSWORD"],
			TrustID:                   raw["TRUST_ID"],
			OctaviaEnabled:            parseBool(raw["OCTAVIA_ENABLED"]),
			ClusterSubnet:             raw["CLUSTER_SUBNET"],
			ExternalNetworkID:         raw["EXTERNAL_NETWORK_ID"],
			ClusterNetworkName:        raw["CLUSTER_NETWORK_NAME"],
			HTTPProxy:                 raw["HTTP_PROXY"],
			HTTPSProxy:                raw["HTTPS_PROXY"],
			NoProxy:                   raw["NO_PROXY"],

			ContainerdVersion:    raw["CONTAINERD_VERSION"],
			ContainerdTarballURL: raw["CONTAINERD_TARBALL_URL"],
			CgroupDriver:         raw["CGROUP_DRIVER"],
			UsePodman:            parseBool(raw["USE_PODMAN"]),
			ContainerInfraPrefix: raw["CONTAINER_INFRA_PREFIX"],

			KubeNodeIP:       raw["KUBE_NODE_IP"],
			KubeNodePublicIP: raw["KUBE_NODE_PUBLIC_IP"],

			PortalNetworkCIDR: raw["PORTAL_NETWORK_CIDR"],
			PodsNetworkCIDR:   raw["PODS_NETWORK_CIDR"],
			FlannelCNITag:     raw["FLANNEL_CNI_TAG"],

			KubeAllowPriv:        raw["KUBE_ALLOW_PRIV"],
			AdmissionControlList: raw["ADMISSION_CONTROL_LIST"],
			KeystoneAuthEnabled:  parseBool(raw["KEYSTONE_AUTH_ENABLED"]),
			CloudProviderEnabled: parseBool(raw["CLOUD_PROVIDER_ENABLED"]),

			CertManagerAPI:               parseBool(raw["CERT_MANAGER_API"]),
			CAKey:                        raw["CA_KEY"],
			KubeServiceAccountKey:        raw["KUBE_SERVICE_ACCOUNT_KEY"],
			KubeServiceAccountPrivateKey: raw["KUBE_SERVICE_ACCOUNT_PRIVATE_KEY"],
			VerifyCA:                     parseBool(raw["VERIFY_CA"]),
			MagnumURL:                    raw["MAGNUM_URL"],

			DNSServiceIP:     raw["DNS_SERVICE_IP"],
			DNSClusterDomain: raw["DNS_CLUSTER_DOMAIN"],

			DockerVolume:        raw["DOCKER_VOLUME"],
			DockerVolumeSize:    parseInt(raw["DOCKER_VOLUME_SIZE"]),
			DockerStorageDriver: raw["DOCKER_STORAGE_DRIVER"],
			EnableCinder:        !parseFalse(raw["ENABLE_CINDER"]),

			InsecureRegistryURL: raw["INSECURE_REGISTRY_URL"],

			KubeletOptions:        raw["KUBELET_OPTIONS"],
			KubeAPIOptions:        raw["KUBEAPI_OPTIONS"],
			KubeControllerOptions: raw["KUBECONTROLLER_OPTIONS"],
			KubeProxyOptions:      raw["KUBEPROXY_OPTIONS"],

			LeadNodeRoleName: raw["LEAD_NODE_ROLE_NAME"],
			KubeImageDigest:  raw["KUBE_IMAGE_DIGEST"],

			HelmClientURL:    raw["HELM_CLIENT_URL"],
			HelmClientSHA256: raw["HELM_CLIENT_SHA256"],
			HelmClientTag:    raw["HELM_CLIENT_TAG"],
			RegionName:       raw["REGION_NAME"],

			FlannelTag:         raw["FLANNEL_TAG"],
			FlannelBackend:     raw["FLANNEL_BACKEND"],
			FlannelNetworkCIDR: raw["FLANNEL_NETWORK_CIDR"],

			CorednsTag: raw["COREDNS_TAG"],

			MetricsServerEnabled:  parseBool(raw["METRICS_SERVER_ENABLED"]),
			MetricsServerChartTag: raw["METRICS_SERVER_CHART_TAG"],

			AutoHealingEnabled:    parseBool(raw["AUTO_HEALING_ENABLED"]),
			AutoHealingController: raw["AUTO_HEALING_CONTROLLER"],

			AutoScalingEnabled: parseBool(raw["AUTO_SCALING_ENABLED"]),
			MinNodeCount:       parseInt(raw["MIN_NODE_COUNT"]),
			MaxNodeCount:       parseInt(raw["MAX_NODE_COUNT"]),

			VolumeDriver:           raw["VOLUME_DRIVER"],
			CinderCSIPluginEnabled: parseBool(raw["CINDER_CSI_PLUGIN_ENABLED"]),
			ManilaCSIPluginEnabled: parseBool(raw["MANILA_CSI_PLUGIN_ENABLED"]),

			PostInstallManifestURL: raw["POST_INSTALL_MANIFEST_URL"],
			MasterIndex:            parseInt(raw["MASTER_INDEX"]),
		},
		Trigger: TriggerConfig{
			CARotationID: strings.TrimSpace(raw["CA_ROTATION_ID"]),
		},
	}

	role := strings.ToLower(strings.TrimSpace(raw["NODEGROUP_ROLE"]))
	switch role {
	case "master", "control-plane":
		cfg.Master = &MasterConfig{
			NumberOfMasters:       parseInt(raw["NUMBER_OF_MASTERS"]),
			KubeAPIPublicAddress:  raw["KUBE_API_PUBLIC_ADDRESS"],
			KubeAPIPrivateAddress: raw["KUBE_API_PRIVATE_ADDRESS"],
			EtcdDiscoveryURL:      raw["ETCD_DISCOVERY_URL"],
			EtcdTag:               raw["ETCD_TAG"],
			MasterHostname:        raw["MASTER_HOSTNAME"],
			EtcdLBVIP:             raw["ETCD_LB_VIP"],
			EtcdVolume:            raw["ETCD_VOLUME"],
			EtcdVolumeSize:        parseInt(raw["ETCD_VOLUME_SIZE"]),
		}
	case "worker", "minion":
		cfg.Worker = &WorkerConfig{
			KubeMasterIP:      raw["KUBE_MASTER_IP"],
			EtcdServerIP:      raw["ETCD_SERVER_IP"],
			RegistryEnabled:   parseBool(raw["REGISTRY_ENABLED"]),
			RegistryPort:      parseInt(raw["REGISTRY_PORT"]),
			RegistryContainer: raw["REGISTRY_CONTAINER"],
			RegistryInsecure:  parseBool(raw["REGISTRY_INSECURE"]),
			RegistryChunksize: parseInt(raw["REGISTRY_CHUNKSIZE"]),
			SwiftRegion:       raw["SWIFT_REGION"],
			TrusteeUsername:   raw["TRUSTEE_USERNAME"],
			TrusteeDomainID:   raw["TRUSTEE_DOMAIN_ID"],
		}
	}

	return cfg, nil
}

func parseHeatParams(content string) (map[string]string, error) {
	raw := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(content))
	lineNo := 0

	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: missing '='", lineNo)
		}

		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		raw[key] = decodeValue(value)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return raw, nil
}

func decodeValue(v string) string {
	if len(v) >= 2 && strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"") {
		unquoted, err := strconv.Unquote(v)
		if err == nil {
			return unquoted
		}
	}

	if len(v) >= 2 && strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") {
		return v[1 : len(v)-1]
	}

	return v
}

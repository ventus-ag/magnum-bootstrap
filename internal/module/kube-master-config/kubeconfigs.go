package kubemasterconfig

import (
	"fmt"
	"strings"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/kubeletconfig"
	"github.com/ventus-ag/magnum-bootstrap/internal/module/kubecommon"
)

func writeKubeConfigs(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change
	certDir := "/etc/kubernetes/certs"
	apiPort := cfg.Shared.KubeAPIPort
	if apiPort == 0 {
		apiPort = 6443
	}

	// Proxy kubeconfig.
	proxyKC := kubecommon.BuildKubeconfig("kube-proxy", certDir+"/proxy.crt", certDir+"/proxy.key",
		certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))
	change, err := executor.EnsureFile("/etc/kubernetes/proxy-kubeconfig.yaml", []byte(proxyKC), 0o640)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Controller kubeconfig.
	controllerKC := kubecommon.BuildKubeconfig("controller", certDir+"/controller.crt", certDir+"/controller.key",
		certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))
	change, err = executor.EnsureFile("/etc/kubernetes/controller-kubeconfig.yaml", []byte(controllerKC), 0o640)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Scheduler kubeconfig.
	schedulerKC := kubecommon.BuildKubeconfig("scheduler", certDir+"/scheduler.crt", certDir+"/scheduler.key",
		certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))
	change, err = executor.EnsureFile("/etc/kubernetes/scheduler-kubeconfig.yaml", []byte(schedulerKC), 0o640)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Kubelet kubeconfig.
	kubeletKC := kubecommon.BuildKubeconfig(fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName),
		certDir+"/kubelet.crt", certDir+"/kubelet.key",
		certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))
	change, err = executor.EnsureFile("/etc/kubernetes/kubelet.conf", []byte(kubeletKC), 0o640)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Keystone webhook config (must be written before API server env
	// so the file exists when the API server starts).
	if cfg.Shared.KeystoneAuthEnabled {
		webhookKC := buildKeystoneWebhookConfig(cfg, certDir, apiPort)
		change, err = executor.EnsureFile("/etc/kubernetes/keystone_webhook_config.yaml", []byte(webhookKC), 0o640)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	// API server env.
	apiServerArgs := buildAPIServerArgs(cfg)
	apiServerEnv := fmt.Sprintf(`KUBE_API_ADDRESS="--bind-address=0.0.0.0 --secure-port=%d"
KUBE_SERVICE_ADDRESSES="--service-cluster-ip-range=%s"
KUBE_API_ARGS="%s"
KUBE_ETCD_SERVERS="--etcd-servers=http://127.0.0.1:2379,http://127.0.0.1:4001"
`, apiPort, cfg.Shared.PortalNetworkCIDR, apiServerArgs)
	change, err = executor.EnsureFile("/etc/kubernetes/apiserver", []byte(apiServerEnv), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Controller manager env.
	controllerArgs := buildControllerManagerArgs(cfg)
	change, err = executor.EnsureFile("/etc/kubernetes/controller-manager",
		[]byte(fmt.Sprintf("KUBE_CONTROLLER_MANAGER_ARGS=\"%s\"\n", controllerArgs)), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Scheduler env.
	schedulerArgs := "--leader-elect=true --kubeconfig=/etc/kubernetes/scheduler-kubeconfig.yaml"
	change, err = executor.EnsureFile("/etc/kubernetes/scheduler",
		[]byte(fmt.Sprintf("KUBE_SCHEDULER_ARGS=\"%s\"\n", schedulerArgs)), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Proxy env.
	proxyArgs := fmt.Sprintf("--kubeconfig=/etc/kubernetes/proxy-kubeconfig.yaml --cluster-cidr=%s --hostname-override=%s %s",
		cfg.Shared.PodsNetworkCIDR, cfg.Shared.InstanceName, cfg.Shared.KubeProxyOptions)
	change, err = executor.EnsureFile("/etc/kubernetes/proxy",
		[]byte(fmt.Sprintf("KUBE_PROXY_ARGS=\"%s\"\n", strings.TrimSpace(proxyArgs))), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Base config.
	change, err = executor.EnsureFile("/etc/kubernetes/config", []byte("KUBE_LOG_LEVEL=\"--v=2\"\n"), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// PodSecurity admission defaults to enforce: "privileged" (allow all).
	// No explicit config needed — the built-in defaults are permissive.
	// Namespace labels (applied in cluster-rbac) provide additional safety.

	return changes, nil
}

func buildAPIServerArgs(cfg config.Config) string {
	certDir := "/etc/kubernetes/certs"

	authzMode := "Node,RBAC"
	if cfg.Shared.KeystoneAuthEnabled {
		authzMode = "Node,Webhook,RBAC"
	}

	args := []string{
		"--runtime-config=api/all=true",
		"--kubelet-preferred-address-types=InternalIP,Hostname,ExternalIP",
	}
	// --allow-privileged was documented as "removed" in K8s 1.27, but the
	// default changed in K8s 1.34+ to disallow privileged containers.
	// Always pass it explicitly to ensure infrastructure DaemonSets
	// (cinder-csi, etc.) that need privileged: true are accepted.
	allowPriv := cfg.Shared.KubeAllowPriv
	if allowPriv == "" {
		allowPriv = "true"
	}
	args = append(args, "--allow-privileged="+allowPriv)
	args = append(args,
		fmt.Sprintf("--authorization-mode=%s", authzMode),
		fmt.Sprintf("--tls-cert-file=%s/server.crt", certDir),
		fmt.Sprintf("--tls-private-key-file=%s/server.key", certDir),
		fmt.Sprintf("--client-ca-file=%s/ca.crt", certDir),
		fmt.Sprintf("--service-account-key-file=%s/service_account.key", certDir),
		fmt.Sprintf("--service-account-signing-key-file=%s/service_account_private.key", certDir),
		"--service-account-issuer=https://kubernetes.default.svc.cluster.local",
		fmt.Sprintf("--kubelet-certificate-authority=%s/ca.crt", certDir),
		fmt.Sprintf("--kubelet-client-certificate=%s/server.crt", certDir),
		fmt.Sprintf("--kubelet-client-key=%s/server.key", certDir),
		fmt.Sprintf("--proxy-client-cert-file=%s/server.crt", certDir),
		fmt.Sprintf("--proxy-client-key-file=%s/server.key", certDir),
		fmt.Sprintf("--requestheader-client-ca-file=%s/ca.crt", certDir),
		"--requestheader-allowed-names=front-proxy-client,kube,kubernetes",
		"--requestheader-extra-headers-prefix=X-Remote-Extra-",
		"--requestheader-group-headers=X-Remote-Group",
		"--requestheader-username-headers=X-Remote-User",
	)
	if cfg.Shared.KeystoneAuthEnabled {
		args = append(args,
			"--authentication-token-webhook-config-file=/etc/kubernetes/keystone_webhook_config.yaml",
			"--authorization-webhook-config-file=/etc/kubernetes/keystone_webhook_config.yaml",
		)
	}
	if cfg.Shared.KubeAPIOptions != "" {
		args = append(args, cfg.Shared.KubeAPIOptions)
	}
	return strings.Join(args, " ")
}

func buildControllerManagerArgs(cfg config.Config) string {
	certDir := "/etc/kubernetes/certs"
	args := []string{
		"--leader-elect=true",
		fmt.Sprintf("--cluster-name=%s", cfg.Shared.ClusterUUID),
		"--allocate-node-cidrs=true",
		"--kubeconfig=/etc/kubernetes/controller-kubeconfig.yaml",
		fmt.Sprintf("--cluster-cidr=%s", cfg.Shared.PodsNetworkCIDR),
		fmt.Sprintf("--service-account-private-key-file=%s/service_account_private.key", certDir),
		fmt.Sprintf("--root-ca-file=%s/ca.crt", certDir),
		"--use-service-account-credentials=true",
	}
	// --secure-port=0 disabled the secure serving port. K8s >= 1.26 deprecated
	// this pattern; keep it only for older versions.
	if !kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 26) {
		args = append(args, "--secure-port=0")
	}
	if cfg.Shared.CloudProviderEnabled {
		args = append(args, "--cloud-provider=external")
	}
	if cfg.Shared.CertManagerAPI {
		args = append(args, fmt.Sprintf("--cluster-signing-cert-file=%s/ca.crt", certDir))
		args = append(args, fmt.Sprintf("--cluster-signing-key-file=%s/ca.key", certDir))
	}
	if cfg.Shared.KubeControllerOptions != "" {
		args = append(args, cfg.Shared.KubeControllerOptions)
	}
	return strings.Join(args, " ")
}

func buildKeystoneWebhookConfig(cfg config.Config, certDir string, apiPort int) string {
	return fmt.Sprintf(`---
apiVersion: v1
clusters:
- cluster:
    certificate-authority: %s/ca.crt
    server: https://127.0.0.1:%d
  name: %s
contexts:
- context:
    cluster: %s
    user: admin
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: admin
  user:
    as-user-extra: {}
    client-certificate: %s/admin.crt
    client-key: %s/admin.key
`, certDir, apiPort, cfg.Shared.ClusterUUID, cfg.Shared.ClusterUUID, certDir, certDir)
}

package kubemasterconfig

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/kubeletconfig"
	"github.com/ventus-ag/magnum-bootstrap/internal/module/kubecommon"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
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
	change, err := applyFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/proxy-kubeconfig.yaml", Content: []byte(proxyKC), Mode: 0o640})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	// Controller kubeconfig.
	controllerKC := kubecommon.BuildKubeconfig("controller", certDir+"/controller.crt", certDir+"/controller.key",
		certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))
	change, err = applyFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/controller-kubeconfig.yaml", Content: []byte(controllerKC), Mode: 0o640})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	// Scheduler kubeconfig.
	schedulerKC := kubecommon.BuildKubeconfig("scheduler", certDir+"/scheduler.crt", certDir+"/scheduler.key",
		certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))
	change, err = applyFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/scheduler-kubeconfig.yaml", Content: []byte(schedulerKC), Mode: 0o640})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	// Kubelet kubeconfig.
	kubeletKC := kubecommon.BuildKubeconfig(fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName),
		certDir+"/kubelet.crt", certDir+"/kubelet.key",
		certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))
	change, err = applyFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/kubelet.conf", Content: []byte(kubeletKC), Mode: 0o640})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	// Keystone webhook config (must be written before API server env
	// so the file exists when the API server starts).
	if cfg.Shared.KeystoneAuthEnabled {
		webhookKC := buildKeystoneWebhookConfig(cfg, certDir, apiPort)
		change, err = applyFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/keystone_webhook_config.yaml", Content: []byte(webhookKC), Mode: 0o640})
		if err != nil {
			return nil, err
		}
		if change.Changed {
			changes = append(changes, change.Changes...)
		}
	}

	// API server env.
	apiServerArgs := buildAPIServerArgs(cfg)
	apiServerEnv := fmt.Sprintf(`KUBE_API_ADDRESS="--bind-address=0.0.0.0 --secure-port=%d"
KUBE_SERVICE_ADDRESSES="--service-cluster-ip-range=%s"
KUBE_API_ARGS="%s"
KUBE_ETCD_SERVERS="--etcd-servers=http://127.0.0.1:2379,http://127.0.0.1:4001"
`, apiPort, cfg.Shared.PortalNetworkCIDR, apiServerArgs)
	change, err = applyFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/apiserver", Content: []byte(apiServerEnv), Mode: 0o644})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	// Controller manager env.
	controllerArgs := buildControllerManagerArgs(cfg)
	change, err = applyFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/controller-manager",
		Content: []byte(fmt.Sprintf("KUBE_CONTROLLER_MANAGER_ARGS=\"%s\"\n", controllerArgs)), Mode: 0o644})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	// Scheduler env.
	schedulerArgs := "--leader-elect=true --kubeconfig=/etc/kubernetes/scheduler-kubeconfig.yaml"
	change, err = applyFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/scheduler",
		Content: []byte(fmt.Sprintf("KUBE_SCHEDULER_ARGS=\"%s\"\n", schedulerArgs)), Mode: 0o644})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	// Proxy env.
	proxyArgs := fmt.Sprintf("--kubeconfig=/etc/kubernetes/proxy-kubeconfig.yaml --cluster-cidr=%s --hostname-override=%s %s",
		cfg.Shared.PodsNetworkCIDR, cfg.Shared.InstanceName, cfg.Shared.KubeProxyOptions)
	change, err = applyFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/proxy",
		Content: []byte(fmt.Sprintf("KUBE_PROXY_ARGS=\"%s\"\n", strings.TrimSpace(proxyArgs))), Mode: 0o644})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	// Base config.
	change, err = applyFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/config", Content: []byte("KUBE_LOG_LEVEL=\"--v=2\"\n"), Mode: 0o644})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	// PodSecurity admission defaults to enforce: "privileged" (allow all).
	// No explicit config needed — the built-in defaults are permissive.
	// Namespace labels (applied in cluster-rbac) provide additional safety.

	return changes, nil
}

func registerKubeConfigResources(ctx *pulumi.Context, name string, cfg config.Config, opts ...pulumi.ResourceOption) error {
	certDir := "/etc/kubernetes/certs"
	apiPort := cfg.Shared.KubeAPIPort
	if apiPort == 0 {
		apiPort = 6443
	}

	resources := []struct {
		name string
		spec hostresource.FileSpec
	}{
		{name: "proxy-kubeconfig", spec: hostresource.FileSpec{Path: "/etc/kubernetes/proxy-kubeconfig.yaml", Content: []byte(kubecommon.BuildKubeconfig("kube-proxy", certDir+"/proxy.crt", certDir+"/proxy.key", certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))), Mode: 0o640}},
		{name: "controller-kubeconfig", spec: hostresource.FileSpec{Path: "/etc/kubernetes/controller-kubeconfig.yaml", Content: []byte(kubecommon.BuildKubeconfig("controller", certDir+"/controller.crt", certDir+"/controller.key", certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))), Mode: 0o640}},
		{name: "scheduler-kubeconfig", spec: hostresource.FileSpec{Path: "/etc/kubernetes/scheduler-kubeconfig.yaml", Content: []byte(kubecommon.BuildKubeconfig("scheduler", certDir+"/scheduler.crt", certDir+"/scheduler.key", certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))), Mode: 0o640}},
		{name: "kubelet-kubeconfig", spec: hostresource.FileSpec{Path: "/etc/kubernetes/kubelet.conf", Content: []byte(kubecommon.BuildKubeconfig(fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName), certDir+"/kubelet.crt", certDir+"/kubelet.key", certDir+"/ca.crt", fmt.Sprintf("https://127.0.0.1:%d", apiPort))), Mode: 0o640}},
		{name: "apiserver-env", spec: hostresource.FileSpec{Path: "/etc/kubernetes/apiserver", Content: []byte(fmt.Sprintf(`KUBE_API_ADDRESS="--bind-address=0.0.0.0 --secure-port=%d"
KUBE_SERVICE_ADDRESSES="--service-cluster-ip-range=%s"
KUBE_API_ARGS="%s"
KUBE_ETCD_SERVERS="--etcd-servers=http://127.0.0.1:2379,http://127.0.0.1:4001"
`, apiPort, cfg.Shared.PortalNetworkCIDR, buildAPIServerArgs(cfg))), Mode: 0o644}},
		{name: "controller-manager-env", spec: hostresource.FileSpec{Path: "/etc/kubernetes/controller-manager", Content: []byte(fmt.Sprintf("KUBE_CONTROLLER_MANAGER_ARGS=\"%s\"\n", buildControllerManagerArgs(cfg))), Mode: 0o644}},
		{name: "scheduler-env", spec: hostresource.FileSpec{Path: "/etc/kubernetes/scheduler", Content: []byte("KUBE_SCHEDULER_ARGS=\"--leader-elect=true --kubeconfig=/etc/kubernetes/scheduler-kubeconfig.yaml\"\n"), Mode: 0o644}},
		{name: "proxy-env", spec: hostresource.FileSpec{Path: "/etc/kubernetes/proxy", Content: []byte(fmt.Sprintf("KUBE_PROXY_ARGS=\"%s\"\n", strings.TrimSpace(fmt.Sprintf("--kubeconfig=/etc/kubernetes/proxy-kubeconfig.yaml --cluster-cidr=%s --hostname-override=%s %s", cfg.Shared.PodsNetworkCIDR, cfg.Shared.InstanceName, cfg.Shared.KubeProxyOptions)))), Mode: 0o644}},
		{name: "base-config", spec: hostresource.FileSpec{Path: "/etc/kubernetes/config", Content: []byte("KUBE_LOG_LEVEL=\"--v=2\"\n"), Mode: 0o644}},
	}
	if cfg.Shared.KeystoneAuthEnabled {
		resources = append(resources, struct {
			name string
			spec hostresource.FileSpec
		}{name: "keystone-webhook", spec: hostresource.FileSpec{Path: "/etc/kubernetes/keystone_webhook_config.yaml", Content: []byte(buildKeystoneWebhookConfig(cfg, certDir, apiPort)), Mode: 0o640}})
	}
	for _, resource := range resources {
		if _, err := hostsdk.RegisterFileSpec(ctx, name+"-"+resource.name, resource.spec, opts...); err != nil {
			return err
		}
	}
	return nil
}

func applyFileResource(executor *host.Executor, spec hostresource.FileSpec) (hostresource.ApplyResult, error) {
	return spec.Apply(executor)
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

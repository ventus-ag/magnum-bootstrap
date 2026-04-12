package kubeworkerconfig

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

	masterIP := ""
	if cfg.Worker != nil {
		masterIP = cfg.Worker.KubeMasterIP
	}
	apiPort := cfg.Shared.KubeAPIPort
	if apiPort == 0 {
		apiPort = 6443
	}
	kubeMasterURI := fmt.Sprintf("https://%s:%d", masterIP, apiPort)

	// Etcd server IP: fall back to master IP if not set.
	etcdServerIP := ""
	if cfg.Worker != nil {
		etcdServerIP = cfg.Worker.EtcdServerIP
		if etcdServerIP == "" {
			etcdServerIP = cfg.Worker.KubeMasterIP
		}
	}

	// Base config.
	// --allow-privileged was removed in K8s 1.27; only include for older versions.
	allowPrivLine := ""
	if !kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 27) {
		allowPrivLine = fmt.Sprintf("\nKUBE_ALLOW_PRIV=\"--allow-privileged=%s\"", cfg.Shared.KubeAllowPriv)
	}
	baseConfig := fmt.Sprintf(`KUBE_LOG_LEVEL="--v=3"%s
KUBE_ETCD_SERVERS="--etcd-servers=http://%s:2379"
KUBE_MASTER="--master=%s"
`, allowPrivLine, etcdServerIP, kubeMasterURI)
	change, err := executor.EnsureFile("/etc/kubernetes/config", []byte(baseConfig), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Kubelet kubeconfig.
	var kubeletKC string
	if cfg.Shared.TLSDisabled {
		kubeletKC = kubecommon.BuildInsecureKubeconfig(
			fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName), kubeMasterURI)
	} else {
		kubeletKC = kubecommon.BuildKubeconfig(
			fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName),
			certDir+"/kubelet.crt", certDir+"/kubelet.key",
			certDir+"/ca.crt", kubeMasterURI)
	}
	change, err = executor.EnsureFile("/etc/kubernetes/kubelet.conf", []byte(kubeletKC), 0o640)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Proxy kubeconfig.
	var proxyKC string
	if cfg.Shared.TLSDisabled {
		proxyKC = kubecommon.BuildInsecureKubeconfig("kube-proxy", kubeMasterURI)
	} else {
		proxyKC = kubecommon.BuildKubeconfig("kube-proxy",
			certDir+"/proxy.crt", certDir+"/proxy.key",
			certDir+"/ca.crt", kubeMasterURI)
	}
	change, err = executor.EnsureFile("/etc/kubernetes/proxy-config.yaml", []byte(proxyKC), 0o640)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Proxy env.
	proxyArgs := fmt.Sprintf("--kubeconfig=/etc/kubernetes/proxy-config.yaml --cluster-cidr=%s --hostname-override=%s %s",
		cfg.Shared.PodsNetworkCIDR, cfg.Shared.InstanceName, cfg.Shared.KubeProxyOptions)
	change, err = executor.EnsureFile("/etc/kubernetes/proxy",
		[]byte(fmt.Sprintf("KUBE_PROXY_ARGS=\"%s\"\n", strings.TrimSpace(proxyArgs))), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Environment.
	change, err = executor.EnsureLine("/etc/environment",
		fmt.Sprintf("KUBERNETES_MASTER=%s", kubeMasterURI), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	return changes, nil
}

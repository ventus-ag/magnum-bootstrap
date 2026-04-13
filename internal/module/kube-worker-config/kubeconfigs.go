package kubeworkerconfig

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
	change, err := applyWorkerFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/config", Content: []byte(baseConfig), Mode: 0o644})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
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
	change, err = applyWorkerFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/kubelet.conf", Content: []byte(kubeletKC), Mode: 0o640})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
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
	change, err = applyWorkerFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/proxy-config.yaml", Content: []byte(proxyKC), Mode: 0o640})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	// Proxy env.
	proxyArgs := fmt.Sprintf("--kubeconfig=/etc/kubernetes/proxy-config.yaml --cluster-cidr=%s --hostname-override=%s %s",
		cfg.Shared.PodsNetworkCIDR, cfg.Shared.InstanceName, cfg.Shared.KubeProxyOptions)
	change, err = applyWorkerFileResource(executor, hostresource.FileSpec{Path: "/etc/kubernetes/proxy",
		Content: []byte(fmt.Sprintf("KUBE_PROXY_ARGS=\"%s\"\n", strings.TrimSpace(proxyArgs))), Mode: 0o644})
	if err != nil {
		return nil, err
	}
	if change.Changed {
		changes = append(changes, change.Changes...)
	}

	// Environment.
	lineResult, err := (hostresource.LineSpec{Path: "/etc/environment", Line: fmt.Sprintf("KUBERNETES_MASTER=%s", kubeMasterURI), Mode: 0o644}).Apply(executor)
	if err != nil {
		return nil, err
	}
	if lineResult.Changed {
		changes = append(changes, lineResult.Changes...)
	}

	return changes, nil
}

func registerKubeConfigResources(ctx *pulumi.Context, name string, cfg config.Config, opts ...pulumi.ResourceOption) error {
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
	etcdServerIP := ""
	if cfg.Worker != nil {
		etcdServerIP = cfg.Worker.EtcdServerIP
		if etcdServerIP == "" {
			etcdServerIP = cfg.Worker.KubeMasterIP
		}
	}
	allowPrivLine := ""
	if !kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 27) {
		allowPrivLine = fmt.Sprintf("\nKUBE_ALLOW_PRIV=\"--allow-privileged=%s\"", cfg.Shared.KubeAllowPriv)
	}
	baseConfig := fmt.Sprintf(`KUBE_LOG_LEVEL="--v=3"%s
KUBE_ETCD_SERVERS="--etcd-servers=http://%s:2379"
KUBE_MASTER="--master=%s"
`, allowPrivLine, etcdServerIP, kubeMasterURI)
	var kubeletKC string
	if cfg.Shared.TLSDisabled {
		kubeletKC = kubecommon.BuildInsecureKubeconfig(fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName), kubeMasterURI)
	} else {
		kubeletKC = kubecommon.BuildKubeconfig(fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName), certDir+"/kubelet.crt", certDir+"/kubelet.key", certDir+"/ca.crt", kubeMasterURI)
	}
	var proxyKC string
	if cfg.Shared.TLSDisabled {
		proxyKC = kubecommon.BuildInsecureKubeconfig("kube-proxy", kubeMasterURI)
	} else {
		proxyKC = kubecommon.BuildKubeconfig("kube-proxy", certDir+"/proxy.crt", certDir+"/proxy.key", certDir+"/ca.crt", kubeMasterURI)
	}
	resources := []struct {
		name string
		spec hostresource.FileSpec
	}{
		{name: "base-config", spec: hostresource.FileSpec{Path: "/etc/kubernetes/config", Content: []byte(baseConfig), Mode: 0o644}},
		{name: "kubelet-kubeconfig", spec: hostresource.FileSpec{Path: "/etc/kubernetes/kubelet.conf", Content: []byte(kubeletKC), Mode: 0o640}},
		{name: "proxy-kubeconfig", spec: hostresource.FileSpec{Path: "/etc/kubernetes/proxy-config.yaml", Content: []byte(proxyKC), Mode: 0o640}},
		{name: "proxy-env", spec: hostresource.FileSpec{Path: "/etc/kubernetes/proxy", Content: []byte(fmt.Sprintf("KUBE_PROXY_ARGS=\"%s\"\n", strings.TrimSpace(fmt.Sprintf("--kubeconfig=/etc/kubernetes/proxy-config.yaml --cluster-cidr=%s --hostname-override=%s %s", cfg.Shared.PodsNetworkCIDR, cfg.Shared.InstanceName, cfg.Shared.KubeProxyOptions)))), Mode: 0o644}},
	}
	for _, resource := range resources {
		if _, err := hostsdk.RegisterFileSpec(ctx, name+"-"+resource.name, resource.spec, opts...); err != nil {
			return err
		}
	}
	_, err := hostsdk.RegisterLineSpec(ctx, name+"-environment", hostresource.LineSpec{Path: "/etc/environment", Line: fmt.Sprintf("KUBERNETES_MASTER=%s", kubeMasterURI), Mode: 0o644}, opts...)
	return err
}

func applyWorkerFileResource(executor *host.Executor, spec hostresource.FileSpec) (hostresource.ApplyResult, error) {
	return spec.Apply(executor)
}

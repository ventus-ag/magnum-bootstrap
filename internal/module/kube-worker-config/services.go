package kubeworkerconfig

import (
	"fmt"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

func writeServiceFiles(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	if !cfg.Shared.UsePodman {
		return nil, nil
	}

	var changes []host.Change
	services := map[string]string{
		"kubelet":    kubeletService(),
		"kube-proxy": kubeProxyService(cfg),
	}

	for name, content := range services {
		path := fmt.Sprintf("/etc/systemd/system/%s.service", name)
		change, err := executor.EnsureFile(path, []byte(content), 0o644)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	if len(changes) > 0 {
		_ = executor.Run("systemctl", "daemon-reload")
	}

	return changes, nil
}

func kubeletService() string {
	return `[Unit]
Description=Kubelet
Wants=rpc-statd.service

[Service]
EnvironmentFile=/etc/sysconfig/heat-params
EnvironmentFile=/etc/kubernetes/config
EnvironmentFile=-/etc/kubernetes/kubelet.env
ExecStartPre=/bin/mkdir -p /etc/kubernetes/cni/net.d
ExecStartPre=/bin/mkdir -p /etc/kubernetes/manifests
ExecStartPre=/bin/mkdir -p /var/lib/calico
ExecStartPre=/bin/mkdir -p /var/lib/containerd
ExecStartPre=/bin/mkdir -p /var/lib/docker
ExecStartPre=/bin/mkdir -p /var/lib/kubelet/volumeplugins
ExecStartPre=/bin/mkdir -p /opt/cni/bin
ExecStart=/usr/local/bin/kubelet \
    $KUBE_LOG_LEVEL $KUBELET_API_SERVER $KUBELET_ADDRESS $KUBELET_PORT $KUBELET_HOSTNAME $KUBELET_ARGS
Delegate=yes
Restart=always
RestartSec=10
TimeoutStartSec=10min
[Install]
WantedBy=multi-user.target
`
}

func kubeProxyService(cfg config.Config) string {
	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/"
	}
	return fmt.Sprintf(`[Unit]
Description=kube-proxy via registry.k8s.io/kube-proxy
[Service]
EnvironmentFile=/etc/sysconfig/heat-params
EnvironmentFile=/etc/kubernetes/config
EnvironmentFile=/etc/kubernetes/proxy
ExecStartPre=/bin/mkdir -p /etc/kubernetes/
ExecStartPre=-/usr/bin/podman rm kube-proxy
ExecStart=/bin/bash -c '/usr/bin/podman run --name kube-proxy \
    --privileged \
    --net host \
    --volume /etc/kubernetes:/etc/kubernetes:ro,z \
    --volume /usr/lib/os-release:/etc/os-release:ro \
    --volume /etc/ssl/certs:/etc/ssl/certs:ro \
    --volume /run:/run \
    --volume /sys/fs/cgroup:/sys/fs/cgroup \
    --volume /lib/modules:/lib/modules:ro \
    --volume /etc/pki/tls/certs:/usr/share/ca-certificates:ro \
    %skube-proxy-%s:%s \
    kube-proxy \
    $KUBE_LOG_LEVEL $KUBE_MASTER $KUBE_PROXY_ARGS'
ExecStop=-/usr/bin/podman stop kube-proxy
Delegate=yes
Restart=always
RestartSec=10
TimeoutStartSec=10min
[Install]
WantedBy=multi-user.target
`, prefix, cfg.Shared.Arch, cfg.Shared.KubeTag)
}

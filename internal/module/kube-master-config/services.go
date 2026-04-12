package kubemasterconfig

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
		"kube-apiserver":          apiServerService(cfg),
		"kube-controller-manager": controllerManagerService(cfg),
		"kube-scheduler":          schedulerService(cfg),
		"kubelet":                 kubeletService(),
		"kube-proxy":              kubeProxyService(cfg),
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

func apiServerService(cfg config.Config) string {
	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/"
	}
	return fmt.Sprintf(`[Unit]
Description=kube-apiserver
After=network.target
[Service]
EnvironmentFile=/etc/sysconfig/heat-params
EnvironmentFile=/etc/kubernetes/config
EnvironmentFile=/etc/kubernetes/apiserver
ExecStartPre=/bin/mkdir -p /etc/kubernetes/
ExecStartPre=-/usr/bin/podman rm kube-apiserver
ExecStart=/bin/bash -c '/usr/bin/podman run --name kube-apiserver \
    --net host \
    --volume /etc/kubernetes:/etc/kubernetes:ro,z \
    --volume /usr/lib/os-release:/etc/os-release:ro \
    --volume /etc/ssl/certs:/etc/ssl/certs:ro \
    --volume /run:/run \
    --volume /etc/pki/tls/certs:/usr/share/ca-certificates:ro \
    %skube-apiserver-%s:%s \
    kube-apiserver \
    $KUBE_LOG_LEVEL $KUBE_ETCD_SERVERS $KUBE_API_ADDRESS $KUBE_SERVICE_ADDRESSES $KUBE_API_ARGS'
ExecStop=-/usr/bin/podman stop kube-apiserver
Delegate=yes
KillMode=process
Restart=always
RestartSec=10
TimeoutStartSec=10min
[Install]
WantedBy=multi-user.target
`, prefix, cfg.Shared.Arch, cfg.Shared.KubeTag)
}

func controllerManagerService(cfg config.Config) string {
	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/"
	}
	return fmt.Sprintf(`[Unit]
Description=kube-controller-manager
After=network.target kube-apiserver.service
[Service]
EnvironmentFile=/etc/sysconfig/heat-params
EnvironmentFile=/etc/kubernetes/config
EnvironmentFile=/etc/kubernetes/controller-manager
ExecStartPre=/bin/mkdir -p /etc/kubernetes/
ExecStartPre=-/usr/bin/podman rm kube-controller-manager
ExecStart=/bin/bash -c '/usr/bin/podman run --name kube-controller-manager \
    --net host \
    --volume /etc/kubernetes:/etc/kubernetes:ro,z \
    --volume /usr/lib/os-release:/etc/os-release:ro \
    --volume /etc/ssl/certs:/etc/ssl/certs:ro \
    --volume /run:/run \
    --volume /etc/pki/tls/certs:/usr/share/ca-certificates:ro \
    %skube-controller-manager-%s:%s \
    kube-controller-manager \
    $KUBE_LOG_LEVEL $KUBE_MASTER $KUBE_CONTROLLER_MANAGER_ARGS'
ExecStop=-/usr/bin/podman stop kube-controller-manager
Delegate=yes
KillMode=process
Restart=always
RestartSec=10
TimeoutStartSec=10min
[Install]
WantedBy=multi-user.target
`, prefix, cfg.Shared.Arch, cfg.Shared.KubeTag)
}

func schedulerService(cfg config.Config) string {
	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/"
	}
	return fmt.Sprintf(`[Unit]
Description=kube-scheduler
After=network.target kube-apiserver.service
[Service]
EnvironmentFile=/etc/sysconfig/heat-params
EnvironmentFile=/etc/kubernetes/config
EnvironmentFile=/etc/kubernetes/scheduler
ExecStartPre=/bin/mkdir -p /etc/kubernetes/
ExecStartPre=-/usr/bin/podman rm kube-scheduler
ExecStart=/bin/bash -c '/usr/bin/podman run --name kube-scheduler \
    --net host \
    --volume /etc/kubernetes:/etc/kubernetes:ro,z \
    --volume /usr/lib/os-release:/etc/os-release:ro \
    --volume /etc/ssl/certs:/etc/ssl/certs:ro \
    --volume /run:/run \
    --volume /etc/pki/tls/certs:/usr/share/ca-certificates:ro \
    %skube-scheduler-%s:%s \
    kube-scheduler \
    $KUBE_LOG_LEVEL $KUBE_MASTER $KUBE_SCHEDULER_ARGS'
ExecStop=-/usr/bin/podman stop kube-scheduler
Delegate=yes
KillMode=process
Restart=always
RestartSec=10
TimeoutStartSec=10min
[Install]
WantedBy=multi-user.target
`, prefix, cfg.Shared.Arch, cfg.Shared.KubeTag)
}

func kubeletService() string {
	return `[Unit]
Description=Kubelet
After=network.target containerd.service
Wants=containerd.service
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
    $KUBE_LOG_LEVEL $KUBE_LOGTOSTDERR $KUBELET_API_SERVER $KUBELET_ADDRESS $KUBELET_HOSTNAME $KUBELET_ARGS
Delegate=yes
KillMode=process
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
Description=kube-proxy
After=network.target
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
KillMode=process
Restart=always
RestartSec=10
TimeoutStartSec=10min
[Install]
WantedBy=multi-user.target
`, prefix, cfg.Shared.Arch, cfg.Shared.KubeTag)
}

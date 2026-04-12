package kubecommon

import (
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

// SetupKubernetesSysctl writes the standard Kubernetes sysctl settings and
// loads the br_netfilter module. Used by both master and worker nodes.
func SetupKubernetesSysctl(executor *host.Executor) ([]host.Change, error) {
	content := `net.ipv4.conf.default.rp_filter=2
net.ipv4.conf.*.rp_filter=2
net.ipv4.conf.all.promote_secondaries = 1
net.ipv4.conf.*.accept_source_route = 1
net.ipv4.ip_unprivileged_port_start = 0
net.ipv4.ping_group_range = 0 2147483647
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward = 1
fs.inotify.max_user_instances = 8192
fs.inotify.max_user_watches = 1048576
`
	// br_netfilter must be loaded for bridge-nf-call-iptables sysctl to work.
	_ = executor.Run("modprobe", "br_netfilter")
	modChange, modErr := executor.EnsureFile("/etc/modules-load.d/k8s-bridge.conf", []byte("br_netfilter\n"), 0o644)
	if modErr != nil {
		return nil, modErr
	}

	change, err := executor.EnsureFile("/etc/sysctl.d/k8s_custom.conf", []byte(content), 0o644)
	if err != nil {
		return nil, err
	}
	var changes []host.Change
	if modChange != nil {
		changes = append(changes, *modChange)
	}
	if change != nil {
		changes = append(changes, *change)
	}
	if len(changes) > 0 {
		_ = executor.Run("sysctl", "--system")
	}
	return changes, nil
}

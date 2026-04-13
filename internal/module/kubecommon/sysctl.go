package kubecommon

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

const kubernetesSysctlContent = `net.ipv4.conf.default.rp_filter=2
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

// SetupKubernetesSysctl writes the standard Kubernetes sysctl settings and
// loads the br_netfilter module. Used by both master and worker nodes.
func SetupKubernetesSysctl(executor *host.Executor) ([]host.Change, error) {
	moduleLoad := hostresource.ModuleLoadSpec{
		Path:    "/etc/modules-load.d/k8s-bridge.conf",
		Modules: []string{"br_netfilter"},
		Mode:    0o644,
	}
	modResult, err := moduleLoad.Apply(executor)
	if err != nil {
		return nil, err
	}

	sysctlConfig := hostresource.SysctlSpec{
		Path:    "/etc/sysctl.d/k8s_custom.conf",
		Content: []byte(kubernetesSysctlContent),
		Mode:    0o644,
	}
	sysctlResult, err := sysctlConfig.Apply(executor)
	if err != nil {
		return nil, err
	}
	var changes []host.Change
	changes = append(changes, modResult.Changes...)
	changes = append(changes, sysctlResult.Changes...)
	return changes, nil
}

func RegisterKubernetesSysctl(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) error {
	moduleLoad := hostresource.ModuleLoadSpec{
		Path:    "/etc/modules-load.d/k8s-bridge.conf",
		Modules: []string{"br_netfilter"},
		Mode:    0o644,
	}
	moduleRes, err := hostsdk.RegisterModuleLoadSpec(ctx, name+"-module-load", moduleLoad, opts...)
	if err != nil {
		return err
	}
	sysctlConfig := hostresource.SysctlSpec{
		Path:    "/etc/sysctl.d/k8s_custom.conf",
		Content: []byte(kubernetesSysctlContent),
		Mode:    0o644,
	}
	sysctlOpts := append([]pulumi.ResourceOption{}, opts...)
	sysctlOpts = append(sysctlOpts, pulumi.DependsOn([]pulumi.Resource{moduleRes}))
	_, err = hostsdk.RegisterSysctlSpec(ctx, name+"-sysctl", sysctlConfig, sysctlOpts...)
	return err
}

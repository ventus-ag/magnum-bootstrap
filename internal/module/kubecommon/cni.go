package kubecommon

import (
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

const (
	CNITag = "v1.6.2"
)

var cniPluginMarkerPaths = []string{
	"/opt/cni/bin/bridge",
	"/opt/cni/bin/host-local",
	"/opt/cni/bin/loopback",
	"/opt/cni/bin/portmap",
}

// SetupFlannelCNI creates CNI directories, downloads and extracts CNI plugins,
// and loads the kernel modules required by flannel.
func SetupFlannelCNI(executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change

	for _, dir := range []string{"/opt/cni/bin", "/srv/magnum/kubernetes/cni"} {
		result, err := (hostresource.DirectorySpec{Path: dir, Mode: 0o755}).Apply(executor)
		if err != nil {
			return nil, err
		}
		changes = append(changes, result.Changes...)
	}

	cniURL := fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/%s/cni-plugins-linux-amd64-%s.tgz",
		CNITag, CNITag)
	cniTgz := fmt.Sprintf("/srv/magnum/kubernetes/cni/cni-plugins-linux-amd64-%s.tgz", CNITag)

	cs, err := ReconcileCNIPlugins(executor, cniURL, cniTgz)
	if err != nil {
		return nil, err
	}
	changes = append(changes, cs...)

	// Kernel modules for flannel.
	moduleLoad := hostresource.ModuleLoadSpec{
		Path:    "/etc/modules-load.d/flannel.conf",
		Modules: []string{"vxlan", "br_netfilter"},
		Mode:    0o644,
	}
	moduleResult, err := moduleLoad.Apply(executor)
	if err != nil {
		return nil, err
	}
	changes = append(changes, moduleResult.Changes...)

	return changes, nil
}

// ReconcileCNIPlugins downloads and extracts CNI plugins if they are not
// already installed. In dry-run mode it reports planned changes.
func ReconcileCNIPlugins(executor *host.Executor, cniURL, cniTgz string) ([]host.Change, error) {
	if CNIPluginsInstalled(cniTgz) {
		return nil, nil
	}

	var changes []host.Change
	if _, err := os.Stat(cniTgz); os.IsNotExist(err) {
		dl, err := (hostresource.DownloadSpec{URL: cniURL, Path: cniTgz, Mode: 0o644, Retries: 5}).Apply(executor)
		if err != nil {
			return nil, fmt.Errorf("download CNI plugins: %w", err)
		}
		changes = append(changes, dl.Changes...)
	} else if err != nil {
		return nil, err
	}

	extract := hostresource.ExtractTarSpec{
		ArchivePath:      cniTgz,
		Destination:      "/opt/cni/bin",
		CheckPaths:       cniPluginMarkerPaths,
		ChmodExecutables: true,
	}
	extractResult, err := extract.Apply(executor)
	if err != nil {
		return nil, fmt.Errorf("extract CNI plugins: %w", err)
	}
	changes = append(changes, extractResult.Changes...)
	return changes, nil
}

func RegisterFlannelCNI(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) error {
	dirResources := map[string]pulumi.Resource{}
	for _, dir := range []string{"/opt/cni/bin", "/srv/magnum/kubernetes/cni"} {
		resourceName := name + "-dir-" + strings.ReplaceAll(strings.Trim(dir, "/"), "/", "-")
		res, err := hostsdk.RegisterDirectorySpec(ctx, resourceName, hostresource.DirectorySpec{Path: dir, Mode: 0o755}, opts...)
		if err != nil {
			return err
		}
		dirResources[dir] = res
	}
	cniURL := fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/%s/cni-plugins-linux-amd64-%s.tgz",
		CNITag, CNITag)
	cniTgz := fmt.Sprintf("/srv/magnum/kubernetes/cni/cni-plugins-linux-amd64-%s.tgz", CNITag)
	downloadOpts := append([]pulumi.ResourceOption{}, opts...)
	downloadOpts = append(downloadOpts, pulumi.DependsOn([]pulumi.Resource{dirResources["/srv/magnum/kubernetes/cni"]}))
	downloadRes, err := hostsdk.RegisterDownloadSpec(ctx, name+"-download", hostresource.DownloadSpec{URL: cniURL, Path: cniTgz, Mode: 0o644, Retries: 5}, downloadOpts...)
	if err != nil {
		return err
	}
	extractOpts := append([]pulumi.ResourceOption{}, opts...)
	extractOpts = append(extractOpts, pulumi.DependsOn([]pulumi.Resource{dirResources["/opt/cni/bin"], downloadRes}))
	if _, err := hostsdk.RegisterExtractTarSpec(ctx, name+"-extract", hostresource.ExtractTarSpec{
		ArchivePath:      cniTgz,
		Destination:      "/opt/cni/bin",
		CheckPaths:       cniPluginMarkerPaths,
		ChmodExecutables: true,
	}, extractOpts...); err != nil {
		return err
	}
	_, err = hostsdk.RegisterModuleLoadSpec(ctx, name+"-module-load", hostresource.ModuleLoadSpec{
		Path:    "/etc/modules-load.d/flannel.conf",
		Modules: []string{"vxlan", "br_netfilter"},
		Mode:    0o644,
	}, opts...)
	return err
}

// CNIPluginsInstalled returns true if the CNI tarball and key binaries exist.
func CNIPluginsInstalled(cniTgz string) bool {
	for _, path := range []string{
		cniTgz,
		"/opt/cni/bin/bridge",
		"/opt/cni/bin/host-local",
		"/opt/cni/bin/loopback",
		"/opt/cni/bin/portmap",
	} {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	return true
}

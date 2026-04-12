package kubecommon

import (
	"context"
	"fmt"
	"os"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

const (
	CNITag = "v1.6.2"
)

// SetupFlannelCNI creates CNI directories, downloads and extracts CNI plugins,
// and loads the kernel modules required by flannel.
func SetupFlannelCNI(executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change

	for _, dir := range []string{"/opt/cni/bin", "/srv/magnum/kubernetes/cni"} {
		change, err := executor.EnsureDir(dir, 0o755)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
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
	_ = executor.Run("modprobe", "-a", "vxlan", "br_netfilter")
	change, err := executor.EnsureFile("/etc/modules-load.d/flannel.conf", []byte("vxlan\nbr_netfilter\n"), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	return changes, nil
}

// ReconcileCNIPlugins downloads and extracts CNI plugins if they are not
// already installed. In dry-run mode it reports planned changes.
func ReconcileCNIPlugins(executor *host.Executor, cniURL, cniTgz string) ([]host.Change, error) {
	if CNIPluginsInstalled(cniTgz) {
		return nil, nil
	}
	if !executor.Apply {
		return []host.Change{{
			Action:  host.ActionCreate,
			Path:    cniTgz,
			Summary: fmt.Sprintf("download CNI plugins from %s", cniURL),
		}}, nil
	}

	var changes []host.Change
	if _, err := os.Stat(cniTgz); os.IsNotExist(err) {
		dl, err := executor.DownloadFileWithRetry(context.Background(), cniURL, cniTgz, 0o644, 5)
		if err != nil {
			return nil, fmt.Errorf("download CNI plugins: %w", err)
		}
		if dl.Change != nil {
			changes = append(changes, *dl.Change)
		}
	} else if err != nil {
		return nil, err
	}

	if err := executor.Run("tar", "-C", "/opt/cni/bin", "-xzf", cniTgz); err != nil {
		return nil, fmt.Errorf("extract CNI plugins: %w", err)
	}
	if err := executor.Run("chmod", "+x", "/opt/cni/bin/."); err != nil {
		return nil, fmt.Errorf("chmod CNI plugins: %w", err)
	}
	changes = append(changes, host.Change{Action: host.ActionUpdate, Path: "/opt/cni/bin", Summary: "extract CNI plugins"})
	return changes, nil
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

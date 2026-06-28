package kubecommon

import "fmt"

// PodmanResetExecStartPre returns the systemd ExecStartPre directives that clear
// a stale or storage-corrupted podman container before the unit (re)creates it.
//
// A plain `podman rm <name>` cannot delete an orphaned storage-layer container.
// When podman's layer metadata is lost the start fails with
//
//	Error: error looking up container "<name>" mounts: layer not known
//	Error: error creating container storage: the container name "<name>" is
//	    already in use by "<id>". You have to remove that container to be able
//	    to reuse that name.: that name is already in use
//
// The container name stays registered in c/storage while libpod can no longer
// see it, so every restart re-hits "name already in use" and the unit
// crash-loops forever (status=125). `rm -f` clears a normally-known container
// (running or stopped); `rm --storage` clears the storage-only orphan that
// libpod has lost track of. Both are best-effort (leading "-") so neither
// blocks ExecStart, mirroring the original single rm line's semantics.
func PodmanResetExecStartPre(podmanPath, name string) string {
	return fmt.Sprintf("ExecStartPre=-%s rm -f %s\nExecStartPre=-%s rm --storage %s",
		podmanPath, name, podmanPath, name)
}

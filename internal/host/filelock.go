package host

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
)

// lockDir holds the advisory lock files used to serialize read-modify-write
// operations on shared host files (notably /etc/fstab, written by both the
// etcd-config and storage phases, and /etc/bashrc, written by the proxy and
// admin-kubeconfig phases). tmpfs, so stale locks vanish on reboot.
const lockDir = "/run/magnum-bootstrap/locks"

// lockSharedFile serializes concurrent read-modify-write access to path.
// It flocks a stable sidecar lock file rather than path itself: our writes
// are atomic renames, and a flock held on the pre-rename inode would not
// exclude a writer that opened the path after the rename. flock covers both
// concurrently scheduled phases in this process and the host-provider plugin
// process operating on the same file during the same pulumi up.
//
// Best-effort: if the lock directory cannot be created (unexpected — /run is
// writable for root on both FCoS and Ubuntu), callers proceed unlocked rather
// than failing the operation, matching the previous behavior.
func lockSharedFile(path string) (release func()) {
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return func() {}
	}
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	lockPath := filepath.Join(lockDir, hex.EncodeToString(sum[:8])+".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return func() {}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

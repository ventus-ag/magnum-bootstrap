// Package fsatomic provides crash-safe file writes for the reconciler's
// bookkeeping files (run journal, applied state). A torn in-place write of
// those files must never survive a power loss: the journal is loaded before
// MarkRunning, so a corrupt file would otherwise fail every future run.
package fsatomic

import (
	"os"
	"path/filepath"
)

// WriteFile writes data to path via a temp file + rename in the same
// directory, fsyncing the file before the rename and the directory after, so
// a crash at any point leaves either the old content or the new content —
// never a truncated file.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

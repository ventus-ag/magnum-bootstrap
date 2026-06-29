package kubecommon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

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
//
// NOTE: on old podman (<= 3.1.x) even `rm --storage` aborts on the missing
// layer ("error unmounting container: layer not known") and cannot self-heal —
// the reconciler's HealPodmanStorageOrphans handles that deeper case.
func PodmanResetExecStartPre(podmanPath, name string) string {
	return fmt.Sprintf("ExecStartPre=-%s rm -f %s\nExecStartPre=-%s rm --storage %s",
		podmanPath, name, podmanPath, name)
}

type storageOrphan struct {
	id   string
	name string
}

// HealPodmanStorageOrphans repairs storage-only podman containers whose name
// collides with a kube service, so the systemd unit can recreate the container.
//
// These orphans arise when podman loses a container's storage layer: the name
// stays registered in c/storage but `podman rm` / `podman rm --storage` abort on
// the missing layer (podman <= 3.1.x), so the unit crash-loops forever on
// `the container name "<n>" is already in use`. The unit ExecStartPre can only
// run podman, which cannot self-heal this; this routine does the deeper repair
// — drop the record from <store>/<driver>-containers/containers.json and remove
// the container's metadata dir — after podman's own removal fails.
//
// Safety: only containers reported with status "storage" by `podman ps
// --external` are touched. A live, libpod-managed container (status Up/Exited)
// is never a storage-only entry, so a healthy node is a no-op and a running
// service is never removed. For each orphan the owning unit (unit name == kube
// container name) is stopped first so podman's Restart=always loop is not
// racing the repair, and `reset-failed` clears the inflated restart counter.
func HealPodmanStorageOrphans(executor *host.Executor, names ...string) ([]host.Change, error) {
	wanted := map[string]bool{}
	for _, n := range names {
		if n != "" {
			wanted[n] = true
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	orphans := listStorageOrphans(executor, wanted)
	if len(orphans) == 0 {
		return nil, nil
	}

	var changes []host.Change
	store := podmanStorePaths(executor)
	for _, o := range orphans {
		// Stop the crash-looping unit so its podman is not respawning while we
		// repair. Best-effort: name may not be a unit, or already inactive.
		_ = executor.Systemctl("stop", o.name)

		// Courtesy: let podman remove it first (clean path for a non-corrupt
		// storage orphan). Ignore errors — the deep repair below is the fallback.
		_ = executor.Run("podman", "rm", "-f", "--storage", o.id)

		if !storageOrphanPresent(executor, o.id) {
			_ = executor.Systemctl("reset-failed", o.name)
			changes = append(changes, host.Change{Action: host.ActionOther,
				Summary: fmt.Sprintf("removed stale podman storage container %s (%s)", o.name, short(o.id))})
			continue
		}

		// Deep repair: podman cannot delete it (missing layer). Scrub the record.
		if !executor.Apply {
			changes = append(changes, host.Change{Action: host.ActionOther,
				Summary: fmt.Sprintf("would scrub corrupt podman storage container %s (%s)", o.name, short(o.id))})
			continue
		}
		scrubbed, err := scrubStorageEntry(store, "containers", o.id)
		if err != nil {
			return changes, fmt.Errorf("heal podman storage orphan %s (%s): %w", o.name, short(o.id), err)
		}
		_ = executor.Systemctl("reset-failed", o.name)
		if scrubbed {
			changes = append(changes, host.Change{Action: host.ActionOther,
				Summary: fmt.Sprintf("scrubbed corrupt podman storage container %s (%s)", o.name, short(o.id))})
		}
	}
	return changes, nil
}

// listStorageOrphans returns storage-only containers whose name matches one of
// the wanted kube service names. `--external` surfaces containers that exist in
// c/storage but not in libpod; those carry status "storage".
func listStorageOrphans(executor *host.Executor, wanted map[string]bool) []storageOrphan {
	out, err := executor.RunCapture("podman", "ps", "-a", "--external", "--no-trunc",
		"--format", "{{.ID}}|{{.Names}}|{{.Status}}")
	if err != nil {
		return nil
	}
	var orphans []storageOrphan
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "|", 3)
		if len(f) != 3 {
			continue
		}
		id := strings.TrimSpace(f[0])
		status := strings.ToLower(strings.TrimSpace(f[2]))
		if status != "storage" {
			continue
		}
		// The names column may list several comma-separated names.
		for _, n := range strings.Split(f[1], ",") {
			if wanted[strings.TrimSpace(n)] {
				orphans = append(orphans, storageOrphan{id: id, name: strings.TrimSpace(n)})
				break
			}
		}
	}
	return orphans
}

func storageOrphanPresent(executor *host.Executor, id string) bool {
	out, err := executor.RunCapture("podman", "ps", "-a", "--external", "--no-trunc", "--format", "{{.ID}}")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == id {
			return true
		}
	}
	return false
}

type storePaths struct {
	graphRoot string
	runRoot   string
	driver    string
}

func podmanStorePaths(executor *host.Executor) storePaths {
	sp := storePaths{graphRoot: "/var/lib/containers/storage", runRoot: "/run/containers/storage", driver: "overlay"}
	out, err := executor.RunCapture("podman", "info", "--format",
		"{{.Store.GraphRoot}}|{{.Store.RunRoot}}|{{.Store.GraphDriverName}}")
	if err == nil {
		f := strings.SplitN(strings.TrimSpace(out), "|", 3)
		if len(f) == 3 {
			if f[0] != "" {
				sp.graphRoot = f[0]
			}
			if f[1] != "" {
				sp.runRoot = f[1]
			}
			if f[2] != "" {
				sp.driver = f[2]
			}
		}
	}
	return sp
}

// HealPodmanCorruptImages removes podman images whose on-disk metadata is
// corrupt, so the kube unit's `podman run` re-pulls a fresh copy on next start.
//
// A lost image manifest makes `podman run <ref>` fail with
//
//	Error: error reading image "<id>" as image: error locating item named
//	    "manifest" for image with ID "<id>": file does not exist
//
// podman resolves the unit's image tag to that cached, unreadable image and
// never re-pulls, so the unit crash-loops (status=125) — the same class of
// c/storage damage as the container orphan, but in the image store. `podman rmi
// -f` clears it even when the manifest is gone; the unit's default run pull
// policy ("missing") then pulls a fresh image. On these nodes podman only holds
// the small control-plane image set (addons run under containerd), so scanning
// every podman image is cheap.
//
// Safety: only images that FAIL `podman image inspect` are removed. A healthy
// image inspects fine, so a healthy node is a no-op and an in-use image (which
// inspects fine and would refuse `rmi`) is never touched.
func HealPodmanCorruptImages(executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change
	store := podmanStorePaths(executor)

	// A single corrupt image makes `podman images` exit non-zero, and podman may
	// abort the listing after the first bad entry — so heal in rounds, re-listing
	// after each removal until the listing is clean or no further progress is
	// made. maxRounds bounds it against an un-healable entry.
	const maxRounds = 8
	for round := 0; round < maxRounds; round++ {
		stdout, stderr, _ := executor.RunCaptureBoth("podman", "images", "-a", "--no-trunc", "--format", "{{.ID}}")
		ids := collectImageIDs(stdout, stderr)
		if len(ids) == 0 {
			return changes, nil
		}

		healed := false
		for _, id := range ids {
			if imageReadable(executor, id) {
				continue
			}

			_ = executor.Run("podman", "rmi", "-f", id)
			if !imageExists(executor, id) {
				changes = append(changes, host.Change{Action: host.ActionOther,
					Summary: fmt.Sprintf("removed corrupt podman image %s", short(id))})
				healed = true
				continue
			}

			// rmi couldn't remove it (manifest gone on old podman): scrub the store.
			if !executor.Apply {
				changes = append(changes, host.Change{Action: host.ActionOther,
					Summary: fmt.Sprintf("would scrub corrupt podman image %s", short(id))})
				continue
			}
			scrubbed, err := scrubStorageEntry(store, "images", id)
			if err != nil {
				return changes, fmt.Errorf("heal corrupt podman image %s: %w", short(id), err)
			}
			if scrubbed {
				changes = append(changes, host.Change{Action: host.ActionOther,
					Summary: fmt.Sprintf("scrubbed corrupt podman image %s", short(id))})
				healed = true
			}
		}

		// Nothing healed this round: either the listing is clean (done) or the
		// remaining corrupt entry can't be removed (avoid spinning). Stop.
		if !healed {
			break
		}
	}
	return changes, nil
}

// imageIDPattern matches a full (untruncated) container image ID: 64 hex chars.
var imageIDPattern = regexp.MustCompile(`[0-9a-f]{64}`)

// collectImageIDs returns the deduped set of image IDs from a
// `podman images --no-trunc --format {{.ID}}` invocation. It parses the good IDs
// from stdout AND any image IDs named on stderr: a corrupt image is omitted from
// stdout but reported on stderr (e.g. `error reading image "<sha>"`), so without
// the stderr scan the corrupt entry — the only one that needs healing — would be
// invisible.
func collectImageIDs(stdout, stderr string) []string {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for _, line := range strings.Split(stdout, "\n") {
		add(line)
	}
	for _, id := range imageIDPattern.FindAllString(stderr, -1) {
		add(id)
	}
	return ids
}

func imageReadable(executor *host.Executor, id string) bool {
	_, err := executor.RunCapture("podman", "image", "inspect", id)
	return err == nil
}

func imageExists(executor *host.Executor, id string) bool {
	return executor.Run("podman", "image", "exists", id) == nil
}

// scrubStorageEntry drops a corrupt c/storage entry (kind = "containers" or
// "images") that podman itself cannot remove, by deleting its record from
// <store>/<driver>-<kind>/<kind>.json and removing its metadata dir. The
// c/storage lock for that store is held across the edit.
func scrubStorageEntry(store storePaths, kind, id string) (bool, error) {
	scrubbed := false
	for _, root := range []string{store.graphRoot, store.runRoot} {
		if root == "" {
			continue
		}
		dir := filepath.Join(root, store.driver+"-"+kind)

		// Hold the c/storage lock across the read-modify-write so a concurrent
		// podman operation can't lose our edit (or vice versa). containers/
		// storage uses fcntl POSIX locks on <kind>.lock, which interoperate with
		// the fcntl lock taken here. Best-effort: if the lock can't be taken the
		// scrub still proceeds (an already-wedged node that needs healing is
		// better repaired racily than not at all).
		err := withStorageLock(dir, kind+".lock", func() error {
			removed, err := removeStorageRecord(filepath.Join(dir, kind+".json"), id)
			if err != nil {
				return err
			}
			if removed {
				scrubbed = true
			}
			entryDir := filepath.Join(dir, id)
			if _, err := os.Stat(entryDir); err == nil {
				if err := os.RemoveAll(entryDir); err != nil {
					return fmt.Errorf("remove %s: %w", entryDir, err)
				}
				scrubbed = true
			}
			return nil
		})
		if err != nil {
			return scrubbed, err
		}
	}
	return scrubbed, nil
}

// withStorageLock runs fn while holding an exclusive fcntl lock on lockName in
// dir, matching containers/storage's own locking so the repair is mutually
// exclusive with concurrent podman operations. If the lock file can't be opened
// or locked (e.g. dir absent), fn runs unlocked.
func withStorageLock(dir, lockName string, fn func() error) error {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fn()
	}
	defer f.Close()

	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLKW, &lk); err == nil {
		defer func() {
			ulk := syscall.Flock_t{Type: syscall.F_UNLCK, Whence: 0, Start: 0, Len: 0}
			_ = syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &ulk)
		}()
	}
	return fn()
}

// removeStorageRecord drops the entry with the given id from a c/storage
// containers.json or images.json (both are arrays of objects keyed by "id").
// Surviving records are kept as raw bytes so their exact shape and field order
// are preserved; only the deleted record is dropped.
func removeStorageRecord(path, id string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	var records []json.RawMessage
	if err := json.Unmarshal(data, &records); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}

	kept := make([]json.RawMessage, 0, len(records))
	removed := false
	for _, raw := range records {
		var meta struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &meta); err == nil && meta.ID == id {
			removed = true
			continue
		}
		kept = append(kept, raw)
	}
	if !removed {
		return false, nil
	}

	out, err := json.Marshal(kept)
	if err != nil {
		return false, err
	}
	tmp := path + ".heal.tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	return true, nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

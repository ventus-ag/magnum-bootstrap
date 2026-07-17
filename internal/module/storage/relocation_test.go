package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeStoreHasData_ContainerdFreshSkeleton(t *testing.T) {
	dir := t.TempDir()
	// A just-started containerd creates the snapshotter skeleton with an
	// empty snapshots dir — must NOT count as live data.
	if err := os.MkdirAll(filepath.Join(dir, "io.containerd.snapshotter.v1.overlayfs", "snapshots"), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtimeStoreHasData(dir, "containerd") {
		t.Fatal("fresh containerd skeleton misdetected as live store")
	}
}

func TestRuntimeStoreHasData_ContainerdLiveStore(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "io.containerd.snapshotter.v1.overlayfs", "snapshots", "42"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !runtimeStoreHasData(dir, "containerd") {
		t.Fatal("populated containerd store not detected")
	}
}

func TestRuntimeStoreHasData_MissingDir(t *testing.T) {
	if runtimeStoreHasData(filepath.Join(t.TempDir(), "nope"), "containerd") {
		t.Fatal("missing dir misdetected as live store")
	}
}

func TestRuntimeStoreHasData_DockerLiveStore(t *testing.T) {
	dir := t.TempDir()
	// Fresh dockerd: containers/ exists but is empty → no data.
	if err := os.MkdirAll(filepath.Join(dir, "containers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtimeStoreHasData(dir, "docker") {
		t.Fatal("fresh docker skeleton misdetected as live store")
	}
	if err := os.MkdirAll(filepath.Join(dir, "containers", "abc123"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !runtimeStoreHasData(dir, "docker") {
		t.Fatal("populated docker store not detected")
	}
}

func TestParseStatStartTick(t *testing.T) {
	// comm with spaces AND parens — the awkward real-world case.
	stat := "1234 (containerd-shim (v2)) S 1 1234 1234 0 -1 4194560 1000 0 0 0 5 3 0 0 20 0 12 0 987654 123456789 500 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0"
	tick, ok := parseStatStartTick(stat)
	if !ok || tick != 987654 {
		t.Fatalf("got tick=%d ok=%v, want 987654 true", tick, ok)
	}
	if _, ok := parseStatStartTick("garbage no parens"); ok {
		t.Fatal("garbage accepted")
	}
	if _, ok := parseStatStartTick("1 (x) S 1 2"); ok {
		t.Fatal("truncated stat accepted")
	}
}

func TestOrphanShims(t *testing.T) {
	shims := []shimProc{
		{PID: 10, StartTick: 100}, // older than runtime → orphan
		{PID: 20, StartTick: 500}, // younger → a pod being created, keep
		{PID: 30, StartTick: 300}, // equal boundary → keep (not strictly older)
	}
	got := orphanShims(shims, 300)
	if len(got) != 1 || got[0].PID != 10 {
		t.Fatalf("got %+v, want only pid 10", got)
	}
	if got := orphanShims(nil, 300); len(got) != 0 {
		t.Fatal("nil shims must yield none")
	}
}

func TestMountpointsUnder(t *testing.T) {
	dir := t.TempDir()
	mounts := dir + "/mounts"
	content := "" +
		"overlay /var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/9/fs overlay rw 0 0\n" +
		"overlay /var/lib/containerd/deep/deeper/rootfs overlay rw 0 0\n" +
		"/dev/vdb /var/lib/containerd xfs rw 0 0\n" +
		"/dev/vda4 /var xfs rw 0 0\n" +
		"tmpfs /var/lib/containerd-other tmpfs rw 0 0\n" + // prefix sibling, must NOT match
		"tmpfs /run/containerd/io.containerd.grpc.v1.cri/sandboxes/abc/shm tmpfs rw 0 0\n"
	if err := os.WriteFile(mounts, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := mountpointsUnder(mounts, "/var/lib/containerd")
	if len(got) != 3 {
		t.Fatalf("got %d mountpoints %v, want 3", len(got), got)
	}
	// Deepest first, the prefix itself last.
	if got[len(got)-1] != "/var/lib/containerd" {
		t.Fatalf("prefix itself must sort last, got %v", got)
	}
	for _, mp := range got {
		if mp == "/var/lib/containerd-other" || mp == "/var" {
			t.Fatalf("unrelated mountpoint matched: %s", mp)
		}
	}
	if got := mountpointsUnder(mounts+"-missing", "/var/lib/containerd"); got != nil {
		t.Fatal("missing mounts file must yield nil")
	}
}

func TestClearDirContents(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := clearDirContents(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dir not emptied: %d entries left", len(entries))
	}
	// Dir itself must survive (mountpoint target).
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("dir itself was removed")
	}
	// Missing dir is a no-op, not an error.
	if err := clearDirContents(filepath.Join(dir, "gone")); err != nil {
		t.Fatal(err)
	}
}

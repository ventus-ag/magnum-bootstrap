package kubecommon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectImageIDs(t *testing.T) {
	const corrupt = "b6a454c5a800d201daacead6ff195ec6049fe6dc086621b0670bca912efaf389"
	const good = "1111111111111111111111111111111111111111111111111111111111111111"

	// Real-world failure: `podman images` prints good IDs on stdout, exits
	// non-zero, and names the corrupt image only on stderr.
	stdout := good + "\n" + good + "\n" // duplicate => deduped
	stderr := `Error: error retrieving label for image "` + corrupt + `": ` +
		`you may need to remove the image to resolve the error: error reading image "` + corrupt +
		`" as image: error locating item named "manifest" for image with ID "` + corrupt + `": file does not exist`

	ids := collectImageIDs(stdout, stderr)

	has := func(want string) bool {
		for _, id := range ids {
			if id == want {
				return true
			}
		}
		return false
	}
	if !has(good) {
		t.Errorf("missing good image id from stdout; got %v", ids)
	}
	if !has(corrupt) {
		t.Errorf("missing corrupt image id from stderr (the heal target); got %v", ids)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 deduped ids, got %d: %v", len(ids), ids)
	}

	// No images at all => empty.
	if got := collectImageIDs("", ""); len(got) != 0 {
		t.Errorf("expected no ids, got %v", got)
	}
}

func TestPodmanResetExecStartPre(t *testing.T) {
	got := PodmanResetExecStartPre("/usr/bin/podman", "kube-controller-manager")
	want := "ExecStartPre=-/usr/bin/podman rm -f kube-controller-manager\n" +
		"ExecStartPre=-/usr/bin/podman rm --storage kube-controller-manager"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRemoveContainerRecord(t *testing.T) {
	const orphan = "0241e758f5bb1398a9ef5dee71af4fd991b8e1b5c7f99718e93a34fa8cb93a36"
	const keep = "1f9da5d9a226000000000000000000000000000000000000000000000000aaaa"

	src := `[{"id":"` + keep + `","names":["kube-controller-manager2"],"layer":"L1"},` +
		`{"id":"` + orphan + `","names":["kube-controller-manager"],"layer":"L2"}]`

	dir := t.TempDir()
	path := filepath.Join(dir, "containers.json")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := removeStorageRecord(path, orphan)
	if err != nil {
		t.Fatalf("removeStorageRecord: %v", err)
	}
	if !removed {
		t.Fatal("expected removed=true")
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var recs []struct {
		ID    string   `json:"id"`
		Names []string `json:"names"`
		Layer string   `json:"layer"`
	}
	if err := json.Unmarshal(out, &recs); err != nil {
		t.Fatalf("result not valid json: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 surviving record, got %d", len(recs))
	}
	if recs[0].ID != keep {
		t.Fatalf("wrong record kept: %s", recs[0].ID)
	}
	// Surviving record must be preserved intact (raw passthrough), not mangled.
	if recs[0].Names[0] != "kube-controller-manager2" || recs[0].Layer != "L1" {
		t.Fatalf("surviving record altered: %+v", recs[0])
	}
}

func TestRemoveContainerRecordAbsent(t *testing.T) {
	src := `[{"id":"aaaa","names":["x"]}]`
	dir := t.TempDir()
	path := filepath.Join(dir, "containers.json")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := removeStorageRecord(path, "doesnotexist")
	if err != nil {
		t.Fatalf("removeStorageRecord: %v", err)
	}
	if removed {
		t.Fatal("expected removed=false when id absent")
	}
	// File must be left byte-for-byte untouched when nothing matched.
	out, _ := os.ReadFile(path)
	if string(out) != src {
		t.Fatalf("file changed on no-op: %s", out)
	}
}

func TestRemoveContainerRecordMissingFile(t *testing.T) {
	removed, err := removeStorageRecord(filepath.Join(t.TempDir(), "nope.json"), "x")
	if err != nil {
		t.Fatalf("missing file must be a no-op, got err: %v", err)
	}
	if removed {
		t.Fatal("expected removed=false for missing file")
	}
}

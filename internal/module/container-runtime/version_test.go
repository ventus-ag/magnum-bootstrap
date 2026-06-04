package containerruntime

import (
	"fmt"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

// TestContainerdLayoutBoundary is the regression guard for the version-map floor
// fix: containerdVersions is sparse (1.31/1.32/1.35), and the bundle layout is
// chosen from the resolved containerd major. k8s <= 1.31 must get containerd 1.x
// (v1 layout, extract to /); k8s >= 1.32 — including the in-between 1.33/1.34
// that used to wrongly floor to the lowest key — must get containerd 2.x (v2
// layout). Every supported minor must also resolve to a non-empty version.
func TestContainerdLayoutBoundary(t *testing.T) {
	for minor := 20; minor <= 36; minor++ {
		tag := fmt.Sprintf("v1.%d.0", minor)
		v := config.LookupByKubeVersion(containerdVersions, tag)
		if v == "" {
			t.Fatalf("%s: empty containerd version", tag)
		}
		major, _, ok := parseContainerdMajor(v)
		if !ok {
			t.Fatalf("%s: unparseable containerd version %q", tag, v)
		}
		wantMajor := 1
		if minor >= 32 {
			wantMajor = 2
		}
		if major != wantMajor {
			t.Errorf("%s -> containerd %s (major %d), want major %d", tag, v, major, wantMajor)
		}
	}
}

func TestContainerdTarballURLLayout(t *testing.T) {
	v1 := containerdTarballURL("1.7.30", false)
	v2 := containerdTarballURL("2.1.6", true)
	if v1 == v2 {
		t.Errorf("v1 and v2 tarball URLs should differ (v1=%s v2=%s)", v1, v2)
	}
	for _, u := range []string{v1, v2} {
		if u == "" {
			t.Error("empty containerd tarball URL")
		}
	}
}

func TestPauseImageResolvesAcrossRange(t *testing.T) {
	if len(pauseImageVersions) == 0 {
		t.Fatal("pauseImageVersions is empty")
	}
	for minor := 20; minor <= 36; minor++ {
		if got := pauseImage(fmt.Sprintf("v1.%d.0", minor)); got == "" {
			t.Errorf("v1.%d: empty pause image", minor)
		}
	}
}

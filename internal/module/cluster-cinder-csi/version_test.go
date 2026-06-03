package clustercindercsi

import (
	"fmt"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

func TestCinderCSIChartResolvesAcrossRange(t *testing.T) {
	if len(cinderCSIChartVersions) == 0 {
		t.Fatal("cinderCSIChartVersions is empty")
	}
	for minor := 20; minor <= 36; minor++ {
		tag := fmt.Sprintf("v1.%d.0", minor)
		if config.LookupByKubeVersion(cinderCSIChartVersions, tag) == "" {
			t.Errorf("empty cinder-csi chart for %s", tag)
		}
	}
}

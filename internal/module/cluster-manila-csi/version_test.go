package clustermanilacsi

import (
	"fmt"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

func TestManilaCSIChartResolvesAcrossRange(t *testing.T) {
	if len(manilaCSIChartVersions) == 0 {
		t.Fatal("manilaCSIChartVersions is empty")
	}
	for minor := 20; minor <= 36; minor++ {
		tag := fmt.Sprintf("v1.%d.0", minor)
		if config.LookupByKubeVersion(manilaCSIChartVersions, tag) == "" {
			t.Errorf("empty manila-csi chart for %s", tag)
		}
	}
}

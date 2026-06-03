package clustergpuoperator

import (
	"fmt"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

func TestGPUOperatorChartResolvesAcrossRange(t *testing.T) {
	if len(gpuOperatorChartVersions) == 0 {
		t.Fatal("gpuOperatorChartVersions is empty")
	}
	for minor := 20; minor <= 36; minor++ {
		tag := fmt.Sprintf("v1.%d.0", minor)
		if config.LookupByKubeVersion(gpuOperatorChartVersions, tag) == "" {
			t.Errorf("empty gpu-operator chart for %s", tag)
		}
	}
}

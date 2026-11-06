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

func TestGPUOperatorChartSupportBoundaries(t *testing.T) {
	for tag, want := range map[string]string{
		"v1.27.0": "v24.9.2",
		"v1.28.0": "v24.9.2",
		"v1.29.0": "v25.3.4",
		"v1.31.0": "v25.3.4",
		"v1.32.0": "v26.3.3",
		"v1.33.0": "v26.7.2",
		"v1.34.0": "v26.7.2",
		"v1.35.6": "v26.7.2",
		"v1.36.2": "v26.7.2",
	} {
		if got := config.LookupByKubeVersion(gpuOperatorChartVersions, tag); got != want {
			t.Errorf("%s: chart %q, want %q", tag, got, want)
		}
	}
}

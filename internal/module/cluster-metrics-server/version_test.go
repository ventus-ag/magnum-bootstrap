package clustermetricsserver

import (
	"fmt"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

func TestMetricsServerChartResolvesAcrossRange(t *testing.T) {
	if len(metricsServerChartVersions) == 0 {
		t.Fatal("metricsServerChartVersions is empty")
	}
	for minor := 20; minor <= 36; minor++ {
		tag := fmt.Sprintf("v1.%d.0", minor)
		if config.LookupByKubeVersion(metricsServerChartVersions, tag) == "" {
			t.Errorf("empty metrics-server chart for %s", tag)
		}
	}
}

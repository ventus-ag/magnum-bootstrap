package clusterautoscaler

import (
	"fmt"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

func TestAutoscalerVersionMapsResolveAcrossRange(t *testing.T) {
	maps := map[string]map[string]string{
		"autoscalerImageTags":     autoscalerImageTags,
		"autoscalerChartVersions": autoscalerChartVersions,
	}
	for name, m := range maps {
		if len(m) == 0 {
			t.Fatalf("%s is empty", name)
		}
		for minor := 20; minor <= 36; minor++ {
			tag := fmt.Sprintf("v1.%d.0", minor)
			if config.LookupByKubeVersion(m, tag) == "" {
				t.Errorf("%s: empty value for %s", name, tag)
			}
		}
	}
}

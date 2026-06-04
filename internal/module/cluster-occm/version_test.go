package clusteroccm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

func TestOCCMVersionMapsResolveAcrossRange(t *testing.T) {
	maps := map[string]map[string]string{
		"occmChartVersions": occmChartVersions,
		"occmImageTags":     occmImageTags,
	}
	for name, m := range maps {
		if len(m) == 0 {
			t.Fatalf("%s is empty", name)
		}
		for key := range m {
			if !strings.HasPrefix(key, "1.") {
				t.Errorf("%s: malformed key %q", name, key)
			}
		}
		for minor := 20; minor <= 36; minor++ {
			tag := fmt.Sprintf("v1.%d.0", minor)
			if config.LookupByKubeVersion(m, tag) == "" {
				t.Errorf("%s: empty value for %s", name, tag)
			}
		}
	}
}

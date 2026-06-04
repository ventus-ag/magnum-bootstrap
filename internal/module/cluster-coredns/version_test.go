package clustercoredns

import (
	"fmt"
	"strings"
	"testing"
)

// TestCorednsVersionMapsResolveAcrossRange guards the per-version maps: every
// supported k8s minor (1.20–1.36) must resolve a non-empty chart + image, and
// every map key must be a well-formed minor.
func TestCorednsVersionMapsResolveAcrossRange(t *testing.T) {
	for _, m := range []map[string]string{corednsChartVersions, corednsImageTags} {
		if len(m) == 0 {
			t.Fatal("version map is empty")
		}
		for key := range m {
			if !strings.HasPrefix(key, "1.") {
				t.Errorf("malformed map key %q (want 1.NN)", key)
			}
		}
	}
	for minor := 20; minor <= 36; minor++ {
		tag := fmt.Sprintf("v1.%d.0", minor)
		if corednsChartDefault(tag) == "" {
			t.Errorf("%s: empty coredns chart version", tag)
		}
		if corednsImageDefault(tag) == "" {
			t.Errorf("%s: empty coredns image tag", tag)
		}
	}
}

// TestCorednsCachePluginByVersion pins the kubeadm behavior: k8s >= 1.32 adds
// "disable success/denial" to the cache plugin to avoid stale rolling-update
// responses; earlier versions do not.
func TestCorednsCachePluginByVersion(t *testing.T) {
	older := corednsCachePlugin("v1.31.0")
	if _, ok := older["configBlock"]; ok {
		t.Errorf("1.31 cache plugin should have no configBlock, got %v", older)
	}
	newer := corednsCachePlugin("v1.32.0")
	cb, _ := newer["configBlock"].(string)
	if !strings.Contains(cb, "disable success") || !strings.Contains(cb, "disable denial") {
		t.Errorf("1.32 cache plugin should disable success+denial, got %v", newer)
	}
}

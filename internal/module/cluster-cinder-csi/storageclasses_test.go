package clustercindercsi

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestDefaultStorageClass(t *testing.T) {
	ours := []liveClass{{Name: scDelete, Default: true}, {Name: scRetain}}
	custom := []liveClass{{Name: "standard", Default: true}, {Name: scDelete}}
	retainDefault := []liveClass{{Name: scDelete}, {Name: scRetain, Default: true}}
	// Two defaults already (ours + the user's): auto must not flip either.
	both := []liveClass{{Name: "standard", Default: true}, {Name: scDelete, Default: true}}
	for _, tc := range []struct {
		setting string
		live    []liveClass
		want    string
		warn    bool
	}{
		{"", nil, scDelete, false},
		{"auto", ours, scDelete, false},
		// A legacy cluster with its own default must not get a second one.
		{"", custom, "", false},
		{"auto", retainDefault, scRetain, false},
		{"", both, scDelete, false},
		{"delete", custom, scDelete, false},
		{"Retain", ours, scRetain, false},
		{"none", ours, "", false},
		{"bogus", custom, "", true},
	} {
		got, warning := defaultStorageClass(tc.setting, tc.live)
		if got != tc.want || (warning != "") != tc.warn {
			t.Errorf("defaultStorageClass(%q, %v) = %q, %q; want %q (warn=%v)", tc.setting, tc.live, got, warning, tc.want, tc.warn)
		}
	}
}

func TestStorageClassValuesKeepChartIdentity(t *testing.T) {
	values := storageClassValues(scRetain)
	if values["enabled"] != false {
		t.Fatal("the chart's own classes must be off: they carry no keep policy")
	}
	docs := strings.Split(values["custom"].(string), "---\n")
	if len(docs) != 2 {
		t.Fatalf("want 2 classes, got %d", len(docs))
	}
	want := map[string]string{scDelete: "Delete", scRetain: "Retain"}
	for _, doc := range docs {
		var sc struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Provisioner          string            `json:"provisioner"`
			ReclaimPolicy        string            `json:"reclaimPolicy"`
			AllowVolumeExpansion bool              `json:"allowVolumeExpansion"`
			Parameters           map[string]string `json:"parameters"`
			VolumeBindingMode    string            `json:"volumeBindingMode"`
		}
		if err := yaml.UnmarshalStrict([]byte(doc), &sc); err != nil {
			t.Fatalf("invalid class YAML: %v\n%s", err, doc)
		}
		// Immutable fields must equal the chart's (2.24-2.36) so the in-place
		// upgrade of an existing release patches rather than replaces.
		if sc.APIVersion != "storage.k8s.io/v1" || sc.Kind != "StorageClass" || sc.Provisioner != cinderProvisioner || sc.ReclaimPolicy != want[sc.Metadata.Name] ||
			sc.Parameters != nil || sc.VolumeBindingMode != "" || !sc.AllowVolumeExpansion {
			t.Errorf("%s diverges from the chart-rendered class: %+v", sc.Metadata.Name, sc)
		}
		if sc.Metadata.Annotations["helm.sh/resource-policy"] != "keep" {
			t.Errorf("%s: missing helm.sh/resource-policy=keep", sc.Metadata.Name)
		}
		isDefault := sc.Metadata.Annotations[defaultClassAnnotation] == "true"
		if isDefault != (sc.Metadata.Name == scRetain) {
			t.Errorf("%s: default annotation = %v", sc.Metadata.Name, isDefault)
		}
	}
	if strings.Contains(storageClassValues("")["custom"].(string), defaultClassAnnotation) {
		t.Error("none must render no default annotation")
	}
}

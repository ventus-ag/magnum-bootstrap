package clusterhelm

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

func TestManifestStorageClasses(t *testing.T) {
	manifest := `---
# Source: openstack-cinder-csi/templates/storageclass.yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: csi-cinder-sc-delete
provisioner: cinder.csi.openstack.org
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: openstack-cinder-csi-nodeplugin
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: csi-cinder-sc-retain
`
	got := ManifestStorageClasses(manifest)
	if want := []string{"csi-cinder-sc-delete", "csi-cinder-sc-retain"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ManifestStorageClasses = %v, want %v", got, want)
	}
}

func TestRestorableObjectsStripsServerFields(t *testing.T) {
	list := `{"kind":"List","items":[{"apiVersion":"storage.k8s.io/v1","kind":"StorageClass",
	  "metadata":{"name":"csi-cinder-sc-delete","uid":"u","resourceVersion":"7","creationTimestamp":"t","managedFields":[{}],
	    "labels":{"app.kubernetes.io/managed-by":"Helm"},
	    "annotations":{"meta.helm.sh/release-name":"cinder-csi","storageclass.kubernetes.io/is-default-class":"true"}},
	  "provisioner":"cinder.csi.openstack.org","reclaimPolicy":"Delete"}]}`
	objs, err := RestorableObjects([]byte(list))
	if err != nil || len(objs) != 1 {
		t.Fatalf("RestorableObjects = %d objs, %v", len(objs), err)
	}
	var obj map[string]any
	_ = json.Unmarshal(objs[0], &obj)
	meta := obj["metadata"].(map[string]any)
	for _, field := range []string{"uid", "resourceVersion", "creationTimestamp", "managedFields"} {
		if _, ok := meta[field]; ok {
			t.Errorf("%s must be dropped", field)
		}
	}
	if meta["annotations"].(map[string]any)["meta.helm.sh/release-name"] != "cinder-csi" {
		t.Error("Helm ownership annotations must survive so a later install adopts the class")
	}
	if obj["provisioner"] != "cinder.csi.openstack.org" {
		t.Error("spec fields must survive")
	}
	single, err := RestorableObjects([]byte(`{"kind":"StorageClass","metadata":{"name":"x","uid":"u"}}`))
	if err != nil || len(single) != 1 {
		t.Fatalf("a single object must be accepted too: %d %v", len(single), err)
	}
}

func TestImportMarkerRoundTrip(t *testing.T) {
	encoded := formatImportMarker("kube-system", "cinder-csi", importMarker{revision: 4, attempts: 2})
	if got := parseImportMarker(encoded); got != (importMarker{revision: 4, attempts: 2}) {
		t.Fatalf("parseImportMarker(%q) = %+v", encoded, got)
	}
	if got := parseImportMarker("kube-system/cinder-csi"); got != (importMarker{revision: 0, attempts: 1}) {
		t.Fatalf("a legacy marker must read as one attempt with an unknown revision, got %+v", got)
	}
	if pair, ok := parseHelmReleasePair(encoded); !ok || pair != (HelmReleasePair{Namespace: "kube-system", Name: "cinder-csi"}) {
		t.Fatalf("parseHelmReleasePair(%q) = %+v %v", encoded, pair, ok)
	}
}

func TestDeleteHelmOwnershipConflictsNeverDeletesStorageClasses(t *testing.T) {
	deleted := DeleteHelmOwnershipConflicts(host.NewExecutor(false, nil), []HelmOwnershipConflict{
		{ResourceKind: "StorageClass", ResourceName: "csi-cinder-sc-delete"},
		{ResourceKind: "ConfigMap", ResourceName: "x", ResourceNamespace: "kube-system"},
	})
	if len(deleted) != 1 || deleted[0].ResourceKind != "ConfigMap" {
		t.Fatalf("deleted = %+v; StorageClasses must be skipped", deleted)
	}
}

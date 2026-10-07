package clustercindercsi

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	clusterhelm "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
)

const (
	scDelete          = "csi-cinder-sc-delete"
	scRetain          = "csi-cinder-sc-retain"
	cinderProvisioner = "cinder.csi.openstack.org"

	defaultClassAnnotation     = "storageclass.kubernetes.io/is-default-class"
	betaDefaultClassAnnotation = "storageclass.beta.kubernetes.io/is-default-class"
)

type liveClass struct {
	Name    string
	Default bool
}

func liveStorageClasses(executor *host.Executor) ([]liveClass, error) {
	out, err := executor.RunCapture("kubectl", "--kubeconfig="+clusterhelm.AdminKubeconfig, "get", "storageclass", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, err
	}
	classes := make([]liveClass, 0, len(list.Items))
	for _, item := range list.Items {
		a := item.Metadata.Annotations
		classes = append(classes, liveClass{
			Name:    item.Metadata.Name,
			Default: a[defaultClassAnnotation] == "true" || a[betaDefaultClassAnnotation] == "true",
		})
	}
	return classes, nil
}

// defaultStorageClass resolves CINDER_CSI_DEFAULT_STORAGE_CLASS to the one of
// our classes that carries the default annotation ("" = neither).
func defaultStorageClass(setting string, live []liveClass) (string, string) {
	switch strings.ToLower(strings.TrimSpace(setting)) {
	case "delete":
		return scDelete, ""
	case "retain":
		return scRetain, ""
	case "none":
		return "", ""
	case "", "auto":
	default:
		return autoDefault(live), fmt.Sprintf("unknown cinder_csi_default_storage_class %q (want auto, delete, retain or none); using auto", setting)
	}
	return autoDefault(live), ""
}

// autoDefault never changes an existing cluster's default: one of ours that is
// already default stays so; otherwise ours becomes default only when nothing
// else is.
func autoDefault(live []liveClass) string {
	var deleteDefault, retainDefault, otherDefault bool
	for _, class := range live {
		if !class.Default {
			continue
		}
		switch class.Name {
		case scDelete:
			deleteDefault = true
		case scRetain:
			retainDefault = true
		default:
			otherDefault = true
		}
	}
	switch {
	case deleteDefault:
		return scDelete
	case retainDefault:
		return scRetain
	case otherDefault:
		return ""
	}
	return scDelete
}

// storageClassValues renders our classes through the chart's
// storageClass.custom: same names, provisioner and reclaim policy as the
// chart's own (so the in-place upgrade of an existing release patches them
// rather than deleting them), plus helm.sh/resource-policy=keep, so no
// uninstall — disable, recovery, or reinstall — ever removes them.
func storageClassValues(defaultName string) map[string]interface{} {
	var docs []string
	for _, class := range []struct{ name, reclaim string }{{scDelete, "Delete"}, {scRetain, "Retain"}} {
		annotations := "    helm.sh/resource-policy: keep\n"
		if class.name == defaultName {
			annotations += "    " + defaultClassAnnotation + ": \"true\"\n"
		}
		docs = append(docs, fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
  annotations:
%sprovisioner: %s
reclaimPolicy: %s
allowVolumeExpansion: true
`, class.name, annotations, cinderProvisioner, class.reclaim))
	}
	return map[string]interface{}{
		"enabled": false,
		"custom":  strings.Join(docs, "---\n"),
	}
}

// keepWhileInUse: see clusterhelm.KeepWhileInUse.
func keepWhileInUse(executor *host.Executor) (bool, string) {
	return clusterhelm.KeepWhileInUse(executor, "cinder-csi", "kube-system", "cinder_csi_enabled",
		"PersistentVolume(s) of "+cinderProvisioner, "get", "pv", "-o",
		`jsonpath={range .items[?(@.spec.csi.driver=="`+cinderProvisioner+`")]}{.metadata.name}{"\n"}{end}`)
}

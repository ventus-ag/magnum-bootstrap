package clusterhelm

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

// AdminKubeconfig is the cluster-admin kubeconfig present on every master.
const AdminKubeconfig = "/etc/kubernetes/admin.conf"

// UninstallRelease runs `helm uninstall` but keeps the release's
// StorageClasses: PVCs and GitOps reference them by name, so deleting one is
// never an acceptable side effect of a recovery step. Every uninstall in the
// reconciler goes through here.
func UninstallRelease(executor *host.Executor, name, namespace string, extraArgs ...string) error {
	if executor == nil {
		return nil
	}
	saved := snapshotReleaseStorageClasses(executor, name, namespace)
	err := executor.Run("helm", append([]string{"uninstall", name, "-n", namespace}, extraArgs...)...)
	restoreStorageClasses(executor, saved)
	return err
}

func snapshotReleaseStorageClasses(executor *host.Executor, name, namespace string) [][]byte {
	if !executor.Apply {
		return nil
	}
	manifest, err := executor.RunCapture("helm", "get", "manifest", name, "-n", namespace)
	if err != nil {
		return nil
	}
	names := ManifestStorageClasses(manifest)
	if len(names) == 0 {
		return nil
	}
	out, err := executor.RunCapture("kubectl", append([]string{"--kubeconfig=" + AdminKubeconfig, "get", "storageclass", "-o", "json", "--ignore-not-found"}, names...)...)
	if err != nil {
		if executor.Logger != nil {
			executor.Logger.Warnf("helm uninstall %s/%s: could not snapshot StorageClasses %v: %v", namespace, name, names, err)
		}
		return nil
	}
	saved, err := RestorableObjects([]byte(out))
	if err != nil && executor.Logger != nil {
		executor.Logger.Warnf("helm uninstall %s/%s: could not parse StorageClasses %v: %v", namespace, name, names, err)
	}
	return saved
}

func restoreStorageClasses(executor *host.Executor, saved [][]byte) {
	for _, obj := range saved {
		var meta struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if json.Unmarshal(obj, &meta) != nil || meta.Metadata.Name == "" {
			continue
		}
		if _, err := executor.RunCapture("kubectl", "--kubeconfig="+AdminKubeconfig, "get", "storageclass", meta.Metadata.Name); err == nil {
			continue // kept by Helm (resource-policy keep) or never removed
		}
		if err := createFromJSON(executor, obj); err != nil {
			if executor.Logger != nil {
				executor.Logger.Warnf("helm uninstall: failed to restore StorageClass %s: %v", meta.Metadata.Name, err)
			}
			continue
		}
		if executor.Logger != nil {
			executor.Logger.Warnf("helm uninstall: restored StorageClass %s removed with its release", meta.Metadata.Name)
		}
	}
}

func createFromJSON(executor *host.Executor, obj []byte) error {
	f, err := os.CreateTemp("", "magnum-sc-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(obj); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return executor.Run("kubectl", "--kubeconfig="+AdminKubeconfig, "create", "-f", f.Name())
}

// ManifestStorageClasses returns the names of the StorageClasses a rendered
// Helm manifest contains.
func ManifestStorageClasses(manifest string) []string {
	var names []string
	for _, doc := range strings.Split(manifest, "\n---") {
		var obj struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if yaml.Unmarshal([]byte(doc), &obj) != nil {
			continue
		}
		if obj.Kind == "StorageClass" && obj.Metadata.Name != "" {
			names = append(names, obj.Metadata.Name)
		}
	}
	return names
}

// RestorableObjects turns `kubectl get -o json` output (one object or a List)
// into objects `kubectl create` accepts: server-owned fields are dropped,
// labels and annotations — including Helm's ownership markers, so a later
// install adopts them — are kept.
func RestorableObjects(data []byte) ([][]byte, error) {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 {
		return nil, nil
	}
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	items := []any{top}
	if list, ok := top["items"].([]any); ok {
		items = list
	}
	var objs [][]byte
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		delete(obj, "status")
		if meta, ok := obj["metadata"].(map[string]any); ok {
			for _, field := range []string{"uid", "resourceVersion", "creationTimestamp", "managedFields", "generation", "selfLink", "deletionTimestamp", "deletionGracePeriodSeconds"} {
				delete(meta, field)
			}
		}
		raw, err := json.Marshal(obj)
		if err != nil {
			return objs, err
		}
		objs = append(objs, raw)
	}
	return objs, nil
}

// KeepWhileInUse decides whether an addon switched off must stay installed
// because live objects still depend on it (removing a CSI driver strands the
// pods using its volumes; removing the cloud controller leaks load balancers
// and blocks Service deletion). kubectlArgs must print one dependent per
// line. Fails closed: an unreadable cluster keeps the release.
func KeepWhileInUse(executor *host.Executor, release, namespace, setting, dependents string, kubectlArgs ...string) (bool, string) {
	// Only a release Pulumi manages is pruned on disable. Keeping an unmanaged
	// (legacy) one would make the reconciler adopt and upgrade it instead.
	if _, err := os.Stat(adoptedMarkerPath(namespace, release)); err != nil {
		return false, ""
	}
	if _, err := executor.RunCapture("helm", "status", release, "-n", namespace); err != nil {
		return false, ""
	}
	out, err := executor.RunCapture("kubectl", append([]string{"--kubeconfig=" + AdminKubeconfig}, kubectlArgs...)...)
	if err != nil {
		return true, fmt.Sprintf("%s=false not applied: could not list %s (%v); keeping %s", setting, dependents, err, release)
	}
	if n := countLines(out); n > 0 {
		return true, fmt.Sprintf("%s=false not applied: %d %s still depend on %s; keeping it until they are removed", setting, n, dependents, release)
	}
	return false, ""
}

func countLines(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

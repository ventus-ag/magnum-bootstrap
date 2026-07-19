package kubecommon

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/kubeletconfig"
)

// NodeManagerKubeconfigPath is the scoped kubeconfig workers use for node
// metadata reconciliation. The kubelet identity in the worker admin.conf is
// blocked by NodeRestriction from modifying taints and node-role labels, so
// workers get a dedicated cert (O=magnum:node-manager) bound to a nodes-patch
// ClusterRole by cluster-rbac.
const NodeManagerKubeconfigPath = "/etc/kubernetes/node-manager.conf"

// MetadataKubeconfig picks the kubeconfig for node metadata operations:
// masters use admin.conf (cluster-admin), workers prefer node-manager.conf
// and fall back to admin.conf (kubelet identity — labels still converge, taint
// changes will be rejected until the node-manager credential exists).
func MetadataKubeconfig(cfg config.Config) string {
	if cfg.Role() != config.RoleMaster {
		if _, err := os.Stat(NodeManagerKubeconfigPath); err == nil {
			return NodeManagerKubeconfigPath
		}
	}
	return "/etc/kubernetes/admin.conf"
}

type nodeDocument struct {
	Metadata struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Taints []nodeTaintDoc `json:"taints"`
	} `json:"spec"`
}

type nodeTaintDoc struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

// EnsureNodeMetadata converges the node's labels and taints against the
// desired state from heat-params. It manages three families:
//   - builtin labels (magnum.openstack.org/*, node-role.kubernetes.io/*)
//   - custom NODE_LABELS (add/update/remove)
//   - custom NODE_TAINTS (add/update/remove)
//
// Removal works via managed-set annotations on the Node object
// (magnum.openstack.org/managed-labels / managed-taints): a key present in
// the managed set but absent from the desired set is deleted. Labels/taints
// not in the managed set are never touched, so control-plane taints, kubelet
// condition taints and third-party labels survive. The managed annotation is
// widened (union) BEFORE mutating and narrowed to the desired set after, so
// a crash mid-run can only leave extra tracked keys, never orphans.
func EnsureNodeMetadata(cfg config.Config, executor *host.Executor, kubectl, kubeconfig string, apply bool, retries int, interval time.Duration) ([]host.Change, error) {
	node, err := fetchNodeWithRetry(cfg, executor, kubectl, kubeconfig, apply, retries, interval)
	if err != nil {
		return nil, err
	}
	nodeName := cfg.Shared.InstanceName
	var changes []host.Change

	run := func(args ...string) error {
		full := append([]string{"--kubeconfig=" + kubeconfig}, args...)
		return executor.Run(kubectl, full...)
	}

	// ---- Labels ----
	desired, remove := desiredNodeLabels(cfg)
	custom := cfg.Shared.NodeLabels
	for key, value := range custom {
		if _, builtin := desired[key]; !builtin {
			desired[key] = value
		}
	}
	managedPrev := parseManagedSet(node.Metadata.Annotations[config.ManagedLabelsAnnotation])
	customKeys := config.SortedLabelKeys(custom)

	// Widen the managed annotation first (crash safety).
	union := unionSets(managedPrev, customKeys)
	unionChanges, err := ensureAnnotation(run, nodeName, node.Metadata.Annotations, config.ManagedLabelsAnnotation, union, apply)
	if err != nil {
		return changes, err
	}
	changes = append(changes, unionChanges...)

	for _, key := range remove {
		if _, ok := node.Metadata.Labels[key]; !ok {
			continue
		}
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: fmt.Sprintf("remove node label %s from %s", key, nodeName)})
		if apply {
			if err := run("label", "node", nodeName, key+"-"); err != nil {
				return changes, err
			}
		}
	}
	// Managed custom labels that left the desired set get deleted.
	for _, key := range managedPrev {
		if _, still := custom[key]; still {
			continue
		}
		if _, ok := node.Metadata.Labels[key]; !ok {
			continue
		}
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: fmt.Sprintf("remove node label %s from %s", key, nodeName)})
		if apply {
			if err := run("label", "node", nodeName, key+"-"); err != nil {
				return changes, err
			}
		}
	}
	for _, key := range config.SortedLabelKeys(desired) {
		value := desired[key]
		if current, ok := node.Metadata.Labels[key]; ok && current == value {
			continue
		}
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: fmt.Sprintf("set node label %s=%q on %s", key, value, nodeName)})
		if apply {
			if err := run("label", "node", nodeName, fmt.Sprintf("%s=%s", key, value), "--overwrite"); err != nil {
				return changes, err
			}
		}
	}
	// Narrow the managed annotation to the now-desired set.
	narrowChanges, err := ensureAnnotation(run, nodeName, node.Metadata.Annotations, config.ManagedLabelsAnnotation, customKeys, apply)
	if err != nil {
		return changes, err
	}
	changes = append(changes, narrowChanges...)

	// ---- Taints ----
	// Runs after labels so that under the kubelet-identity fallback (no
	// node-manager credential yet) label convergence still completes before
	// the first Forbidden taint call aborts.
	desiredTaints := cfg.Shared.NodeTaints
	currentTaints := map[string]nodeTaintDoc{}
	for _, taint := range node.Spec.Taints {
		currentTaints[taint.Key+":"+taint.Effect] = taint
	}
	managedTaintsPrev := parseManagedSet(node.Metadata.Annotations[config.ManagedTaintsAnnotation])
	desiredTaintIDs := make([]string, 0, len(desiredTaints))
	desiredTaintSet := map[string]bool{}
	for _, taint := range desiredTaints {
		id := taint.Key + ":" + taint.Effect
		desiredTaintIDs = append(desiredTaintIDs, id)
		desiredTaintSet[id] = true
	}
	sort.Strings(desiredTaintIDs)

	unionTaints := unionSets(managedTaintsPrev, desiredTaintIDs)
	unionChanges, err = ensureAnnotation(run, nodeName, node.Metadata.Annotations, config.ManagedTaintsAnnotation, unionTaints, apply)
	if err != nil {
		return changes, err
	}
	changes = append(changes, unionChanges...)

	for _, id := range managedTaintsPrev {
		if desiredTaintSet[id] {
			continue
		}
		if _, present := currentTaints[id]; !present {
			continue
		}
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: fmt.Sprintf("remove node taint %s from %s", id, nodeName)})
		if apply {
			if err := run("taint", "node", nodeName, id+"-"); err != nil {
				return changes, err
			}
		}
	}
	for _, taint := range desiredTaints {
		id := taint.Key + ":" + taint.Effect
		if current, ok := currentTaints[id]; ok && current.Value == taint.Value {
			continue
		}
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: fmt.Sprintf("set node taint %s on %s", taint.String(), nodeName)})
		if apply {
			if err := run("taint", "node", nodeName, taint.String(), "--overwrite"); err != nil {
				return changes, err
			}
		}
	}
	narrowChanges, err = ensureAnnotation(run, nodeName, node.Metadata.Annotations, config.ManagedTaintsAnnotation, desiredTaintIDs, apply)
	if err != nil {
		return changes, err
	}
	changes = append(changes, narrowChanges...)

	return changes, nil
}

// ensureAnnotation converges one managed-set annotation to the given keys
// (sorted csv). Empty set removes the annotation.
func ensureAnnotation(run func(args ...string) error, nodeName string, annotations map[string]string, key string, keys []string, apply bool) ([]host.Change, error) {
	sorted := append([]string{}, keys...)
	sort.Strings(sorted)
	desired := strings.Join(sorted, ",")
	current, exists := annotations[key]
	var changes []host.Change
	switch {
	case desired == "" && exists:
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: fmt.Sprintf("remove node annotation %s from %s", key, nodeName)})
		if apply {
			if err := run("annotate", "node", nodeName, key+"-"); err != nil {
				return changes, err
			}
		}
	case desired != "" && (!exists || current != desired):
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: fmt.Sprintf("set node annotation %s on %s", key, nodeName)})
		if apply {
			if err := run("annotate", "node", nodeName, fmt.Sprintf("%s=%s", key, desired), "--overwrite"); err != nil {
				return changes, err
			}
		}
	}
	if exists && desired == current {
		return nil, nil
	}
	// Keep the in-memory view current so the narrow pass compares against
	// what we just wrote instead of the stale fetch.
	if desired == "" {
		delete(annotations, key)
	} else {
		annotations[key] = desired
	}
	return changes, nil
}

func parseManagedSet(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func unionSets(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, items := range [][]string{a, b} {
		for _, item := range items {
			if !seen[item] {
				seen[item] = true
				out = append(out, item)
			}
		}
	}
	sort.Strings(out)
	return out
}

func fetchNodeWithRetry(cfg config.Config, executor *host.Executor, kubectl, kubeconfig string, apply bool, retries int, interval time.Duration) (*nodeDocument, error) {
	if retries < 1 {
		retries = 1
	}
	var lastErr error
	for attempt := 1; attempt <= retries; attempt++ {
		node, err := fetchNode(cfg, executor, kubectl, kubeconfig)
		if err == nil {
			return node, nil
		}
		lastErr = err
		if !apply || attempt == retries {
			break
		}
		time.Sleep(interval)
	}
	return nil, lastErr
}

func fetchNode(cfg config.Config, executor *host.Executor, kubectl, kubeconfig string) (*nodeDocument, error) {
	out, err := executor.RunCapture(kubectl, "--kubeconfig="+kubeconfig, "get", "node", cfg.Shared.InstanceName, "-o", "json")
	if err != nil {
		return nil, err
	}
	var node nodeDocument
	if err := json.Unmarshal([]byte(out), &node); err != nil {
		return nil, err
	}
	if node.Metadata.Labels == nil {
		node.Metadata.Labels = map[string]string{}
	}
	if node.Metadata.Annotations == nil {
		node.Metadata.Annotations = map[string]string{}
	}
	return &node, nil
}

func desiredNodeLabels(cfg config.Config) (map[string]string, []string) {
	desired := map[string]string{
		"magnum.openstack.org/role": cfg.Shared.NodegroupRole,
	}
	if cfg.Shared.NodegroupName != "" {
		desired["magnum.openstack.org/nodegroup"] = cfg.Shared.NodegroupName
	}
	remove := make([]string, 0, 2)
	if cfg.Role() == config.RoleMaster {
		if kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 25) {
			desired["node-role.kubernetes.io/control-plane"] = ""
			remove = append(remove, "node-role.kubernetes.io/master")
		} else if kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 20) {
			desired["node-role.kubernetes.io/master"] = ""
			desired["node-role.kubernetes.io/control-plane"] = ""
		} else {
			desired["node-role.kubernetes.io/master"] = ""
			remove = append(remove, "node-role.kubernetes.io/control-plane")
		}
	} else {
		// Workers get a visible ROLES column entry. NodeRestriction blocks the
		// kubelet from self-setting node-role.* — applied via node-manager
		// (or master admin) credentials only.
		desired["node-role.kubernetes.io/"+workerRoleName(cfg.Shared.NodegroupRole)] = ""
	}
	return desired, remove
}

func workerRoleName(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" || role == "minion" {
		return "worker"
	}
	return role
}

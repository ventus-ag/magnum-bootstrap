package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// NodeTaint is one Kubernetes node taint requested for this node via the
// per-nodegroup NODE_TAINTS heat-param.
type NodeTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

// String renders the taint in the canonical kubectl form: key[=value]:Effect.
func (t NodeTaint) String() string {
	if t.Value == "" {
		return t.Key + ":" + t.Effect
	}
	return t.Key + "=" + t.Value + ":" + t.Effect
}

// NODE_LABELS / NODE_TAINTS use ";" as the item separator because the value
// travels inside a Magnum label whose own CLI syntax splits on ",".
const nodeMetadataSeparator = ";"

var validTaintEffects = map[string]bool{
	"NoSchedule":       true,
	"PreferNoSchedule": true,
	"NoExecute":        true,
}

// Label/taint keys are qualified names: [prefix/]name where prefix is a DNS
// subdomain (max 253) and name is max 63 chars of alphanumerics with -_.
// inside. Values are max 63 chars, may be empty.
var (
	qualifiedNameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)
	labelValueRe    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)
	dnsSubdomainRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
)

// reservedLabelKeys are labels owned by the reconciler itself; user input may
// never override them.
var reservedLabelKeys = map[string]bool{
	"magnum.openstack.org/role":      true,
	"magnum.openstack.org/nodegroup": true,
	ManagedLabelsAnnotation:          true,
	ManagedTaintsAnnotation:          true,
}

// Annotation keys used to track which labels/taints this reconciler manages,
// so removals from the desired set can be converged (deleted) later.
const (
	ManagedLabelsAnnotation = "magnum.openstack.org/managed-labels"
	ManagedTaintsAnnotation = "magnum.openstack.org/managed-taints"
)

func validLabelKey(key string) error {
	prefix, name, found := strings.Cut(key, "/")
	if !found {
		name = key
		prefix = ""
	}
	if found {
		if prefix == "" || len(prefix) > 253 || !dnsSubdomainRe.MatchString(prefix) {
			return fmt.Errorf("invalid key prefix %q", prefix)
		}
	}
	if name == "" || !qualifiedNameRe.MatchString(name) {
		return fmt.Errorf("invalid key name %q", name)
	}
	return nil
}

// ParseNodeLabels parses a NODE_LABELS heat-param value of the form
// "k1=v1;k2=v2" into a validated label map. Invalid entries are skipped and
// reported as warnings — a single bad label must not wedge node convergence.
func ParseNodeLabels(raw string) (map[string]string, []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	labels := map[string]string{}
	var warnings []string
	for _, item := range strings.Split(raw, nodeMetadataSeparator) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		key, value, _ := strings.Cut(item, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if err := validLabelKey(key); err != nil {
			warnings = append(warnings, fmt.Sprintf("NODE_LABELS: skipping %q: %v", item, err))
			continue
		}
		if reservedLabelKeys[key] {
			warnings = append(warnings, fmt.Sprintf("NODE_LABELS: skipping reserved key %q", key))
			continue
		}
		if value != "" && !labelValueRe.MatchString(value) {
			warnings = append(warnings, fmt.Sprintf("NODE_LABELS: skipping %q: invalid value", item))
			continue
		}
		labels[key] = value
	}
	if len(labels) == 0 {
		return nil, warnings
	}
	return labels, warnings
}

// ParseNodeTaints parses a NODE_TAINTS heat-param value of the form
// "key=value:NoSchedule;key2:NoExecute" into validated taints. Invalid
// entries are skipped and reported as warnings.
func ParseNodeTaints(raw string) ([]NodeTaint, []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var taints []NodeTaint
	var warnings []string
	seen := map[string]bool{}
	for _, item := range strings.Split(raw, nodeMetadataSeparator) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		body, effect, found := cutLast(item, ":")
		if !found || !validTaintEffects[effect] {
			warnings = append(warnings, fmt.Sprintf("NODE_TAINTS: skipping %q: effect must be one of NoSchedule, PreferNoSchedule, NoExecute", item))
			continue
		}
		key, value, _ := strings.Cut(body, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if err := validLabelKey(key); err != nil {
			warnings = append(warnings, fmt.Sprintf("NODE_TAINTS: skipping %q: %v", item, err))
			continue
		}
		// node-role.* and kubernetes.io node condition taints are owned by the
		// control plane / kubelet; user taints must not collide with them.
		if strings.HasPrefix(key, "node-role.kubernetes.io/") || strings.HasPrefix(key, "node.kubernetes.io/") {
			warnings = append(warnings, fmt.Sprintf("NODE_TAINTS: skipping reserved key %q", key))
			continue
		}
		if value != "" && !labelValueRe.MatchString(value) {
			warnings = append(warnings, fmt.Sprintf("NODE_TAINTS: skipping %q: invalid value", item))
			continue
		}
		id := key + ":" + effect
		if seen[id] {
			warnings = append(warnings, fmt.Sprintf("NODE_TAINTS: skipping duplicate %q", id))
			continue
		}
		seen[id] = true
		taints = append(taints, NodeTaint{Key: key, Value: value, Effect: effect})
	}
	return taints, warnings
}

func cutLast(s, sep string) (before, after string, found bool) {
	idx := strings.LastIndex(s, sep)
	if idx < 0 {
		return s, "", false
	}
	return s[:idx], s[idx+len(sep):], true
}

// KubeletSafeLabels splits custom labels into those the kubelet may pass via
// --node-labels and those that must be applied through the API instead. The
// kubelet refuses to self-register labels in the kubernetes.io / k8s.io
// namespaces outside its allow-list (notably node-role.kubernetes.io/*).
func KubeletSafeLabels(labels map[string]string) (safe, apiOnly map[string]string) {
	safe = map[string]string{}
	apiOnly = map[string]string{}
	for key, value := range labels {
		if kubeletForbiddenLabelKey(key) {
			apiOnly[key] = value
		} else {
			safe[key] = value
		}
	}
	return safe, apiOnly
}

func kubeletForbiddenLabelKey(key string) bool {
	prefix, _, found := strings.Cut(key, "/")
	if !found {
		return false
	}
	inK8sNamespace := prefix == "kubernetes.io" || prefix == "k8s.io" ||
		strings.HasSuffix(prefix, ".kubernetes.io") || strings.HasSuffix(prefix, ".k8s.io")
	if !inK8sNamespace {
		return false
	}
	// Kubelet allow-list namespaces (kubelet.kubernetes.io and
	// node.kubernetes.io subdomains) may still go through --node-labels.
	allowed := prefix == "kubelet.kubernetes.io" || prefix == "node.kubernetes.io" ||
		strings.HasSuffix(prefix, ".kubelet.kubernetes.io") || strings.HasSuffix(prefix, ".node.kubernetes.io")
	return !allowed
}

// SortedLabelKeys returns the map's keys in deterministic order for stable
// rendering (kubelet args, annotations) so reruns produce no spurious drift.
func SortedLabelKeys(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

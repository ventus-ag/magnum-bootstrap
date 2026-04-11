package kubeletconfig

import "regexp"

var kubeTagPattern = regexp.MustCompile(`^v?(\d+)\.(\d+)`)

// FeatureGatesYAML returns the kubelet feature-gate YAML fragment to render
// for the requested Kubernetes version. Older Magnum templates disabled
// GracefulNodeShutdown, but kubelet 1.35+ enables dependent gates by default,
// so carrying that override forward makes kubelet panic on startup.
func FeatureGatesYAML(kubeTag string) string {
	if shouldDisableGracefulNodeShutdown(kubeTag) {
		return "featureGates:\n  GracefulNodeShutdown: false\n"
	}
	return ""
}

func shouldDisableGracefulNodeShutdown(kubeTag string) bool {
	major, minor, ok := ParseKubeMajorMinor(kubeTag)
	if !ok {
		return false
	}
	if major != 1 {
		return false
	}
	return minor > 21 && minor < 35
}

// ParseKubeMajorMinor extracts the major and minor version numbers from a
// Kubernetes version string like "v1.32.0" or "1.32.0".
func ParseKubeMajorMinor(kubeTag string) (int, int, bool) {
	matches := kubeTagPattern.FindStringSubmatch(kubeTag)
	if len(matches) != 3 {
		return 0, 0, false
	}
	major, ok := parseDecimal(matches[1])
	if !ok {
		return 0, 0, false
	}
	minor, ok := parseDecimal(matches[2])
	if !ok {
		return 0, 0, false
	}
	return major, minor, true
}

// KubeMinorAtLeast returns true if kubeTag parses to Kubernetes 1.N where N >= minMinor.
func KubeMinorAtLeast(kubeTag string, minMinor int) bool {
	major, minor, ok := ParseKubeMajorMinor(kubeTag)
	if !ok {
		return false
	}
	return major == 1 && minor >= minMinor
}

func parseDecimal(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	result := 0
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		result = result*10 + int(ch-'0')
	}
	return result, true
}

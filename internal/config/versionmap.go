package config

import (
	"sort"
	"strconv"
	"strings"
)

// LookupByKubeVersion finds the value in versionMap that matches the given
// Kubernetes version string (e.g. "v1.31.5" or "1.31.5"). Map keys are
// "major.minor" strings (e.g. "1.31").
//
// If the exact minor version is not in the map:
//   - versions above the highest key return the highest key's value
//   - versions below the lowest key return the lowest key's value
//   - versions between two keys return the nearest lower key's value (floor)
//
// The floor rule matters for sparse maps (e.g. containerd, which only pins
// 1.31/1.32/1.35): k8s 1.33 must resolve to the 1.32 entry, not silently fall
// back to the lowest key — the previous behavior gave 1.33/1.34 the 1.7.x (v1
// layout) containerd instead of the intended 2.x.
func LookupByKubeVersion(versionMap map[string]string, kubeVersion string) string {
	val, _ := LookupByKubeVersionClamped(versionMap, kubeVersion)
	return val
}

// LookupByKubeVersionClamped is LookupByKubeVersion plus a clampedBelow flag:
// true when the requested version sits BELOW the lowest map entry, i.e. the
// returned value was designed for a newer Kubernetes than requested (e.g. the
// OCCM/CSI charts, whose maps start at 1.24, on a v1.20 cluster). Callers use
// it to log the untested pairing instead of deploying it silently.
func LookupByKubeVersionClamped(versionMap map[string]string, kubeVersion string) (string, bool) {
	requested, ok := parseMajorMinor(kubeVersion)
	if !ok {
		return boundaryValue(versionMap, false), true
	}

	if val, exact := versionMap[strconv.Itoa(requested.major)+"."+strconv.Itoa(requested.minor)]; exact {
		return val, false
	}

	type entry struct {
		ver majorMinor
		key string
	}
	entries := make([]entry, 0, len(versionMap))
	for k := range versionMap {
		if v, ok := parseMajorMinor(k); ok {
			entries = append(entries, entry{ver: v, key: k})
		}
	}
	if len(entries) == 0 {
		return "", false
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ver.less(entries[j].ver) })

	if entries[len(entries)-1].ver.less(requested) {
		return versionMap[entries[len(entries)-1].key], false
	}
	if requested.less(entries[0].ver) {
		return versionMap[entries[0].key], true
	}
	// Nearest key <= requested (floor). Entries are sorted ascending, so walk
	// up keeping the highest key that does not exceed the requested minor.
	chosen := entries[0].key
	for _, e := range entries {
		if !requested.less(e.ver) {
			chosen = e.key
		} else {
			break
		}
	}
	return versionMap[chosen], false
}

// majorMinor compares Kubernetes versions as integer pairs. A float compare
// would misorder single- vs double-digit minors ("1.9" > "1.31" as floats).
type majorMinor struct {
	major, minor int
}

func (a majorMinor) less(b majorMinor) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	return a.minor < b.minor
}

func parseMajorMinor(version string) (majorMinor, bool) {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return majorMinor{}, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return majorMinor{}, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return majorMinor{}, false
	}
	return majorMinor{major: major, minor: minor}, true
}

// boundaryValue returns the highest (upper=true) or lowest (upper=false)
// entry from the map. Used as a last-resort fallback.
func boundaryValue(m map[string]string, upper bool) string {
	type entry struct {
		ver majorMinor
		val string
	}
	var entries []entry
	for k, v := range m {
		if ver, ok := parseMajorMinor(k); ok {
			entries = append(entries, entry{ver: ver, val: v})
		}
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ver.less(entries[j].ver) })
	if upper {
		return entries[len(entries)-1].val
	}
	return entries[0].val
}

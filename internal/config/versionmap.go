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
func LookupByKubeVersion(versionMap map[string]string, kubeVersion string) string {
	v := strings.TrimPrefix(kubeVersion, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return boundaryValue(versionMap, false)
	}

	minor := parts[0] + "." + parts[1]
	if val, ok := versionMap[minor]; ok {
		return val
	}

	// Parse the requested minor as a float for comparison.
	requested, err := strconv.ParseFloat(minor, 64)
	if err != nil {
		return boundaryValue(versionMap, false)
	}

	// Collect and sort all map keys numerically.
	type entry struct {
		num float64
		key string
	}
	entries := make([]entry, 0, len(versionMap))
	for k := range versionMap {
		n, err := strconv.ParseFloat(k, 64)
		if err != nil {
			continue
		}
		entries = append(entries, entry{num: n, key: k})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].num < entries[j].num })

	if requested > entries[len(entries)-1].num {
		return versionMap[entries[len(entries)-1].key]
	}
	return versionMap[entries[0].key]
}

// boundaryValue returns the highest (upper=true) or lowest (upper=false)
// entry from the map. Used as a last-resort fallback.
func boundaryValue(m map[string]string, upper bool) string {
	type entry struct {
		num float64
		val string
	}
	var entries []entry
	for k, v := range m {
		n, err := strconv.ParseFloat(k, 64)
		if err != nil {
			continue
		}
		entries = append(entries, entry{num: n, val: v})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].num < entries[j].num })
	if upper {
		return entries[len(entries)-1].val
	}
	return entries[0].val
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/clustertemplates"
)

// conformanceLeg is one entry of the Sonobuoy conformance matrix: a Kubernetes
// version to test and how to create a cluster at that version. Template is the
// Magnum cluster template to create from; KubeTag is the kube_tag label override
// applied on top (merge_labels) so a version NEWER than any pinned template can
// still be tested by reusing an existing template and appending the new kube_tag.
// KubeTag is empty when Template already pins exactly Version (no override
// needed). The workflow turns []conformanceLeg into a GitHub Actions matrix.
type conformanceLeg struct {
	Version  string `json:"version"`
	Template string `json:"template"`
	KubeTag  string `json:"kubeTag"`
	// Slug is Version with dots replaced by dashes, safe for Magnum cluster /
	// Heat stack / artifact names (which the workflow builds from name_suffix).
	Slug string `json:"slug"`
}

// semverRe matches a plain "vMAJOR.MINOR.PATCH" version (Fedora CoreOS pinned
// templates are named exactly this; Ubuntu templates carry a "-uNN" suffix and
// so are excluded from conformance base selection).
var semverRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// parseKubeVersion parses "v1.36.2" into its numeric components. ok is false for
// anything that is not a plain vX.Y.Z (e.g. "v1.29.14-u22", "", "latest").
func parseKubeVersion(s string) (major, minor, patch int, ok bool) {
	m := semverRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	patch, _ = strconv.Atoi(m[3])
	return major, minor, patch, true
}

// cmpKubeVersion orders two vX.Y.Z strings: -1 if a<b, 0 if equal/unparseable,
// +1 if a>b. Unparseable versions sort as lowest.
func cmpKubeVersion(a, b string) int {
	amaj, amin, apat, aok := parseKubeVersion(a)
	bmaj, bmin, bpat, bok := parseKubeVersion(b)
	if !aok || !bok {
		switch {
		case aok && !bok:
			return 1
		case !aok && bok:
			return -1
		default:
			return 0
		}
	}
	for _, d := range []int{amaj - bmaj, amin - bmin, apat - bpat} {
		if d != 0 {
			if d < 0 {
				return -1
			}
			return 1
		}
	}
	return 0
}

// templateInfo is the conformance-relevant view of a Magnum cluster template.
type templateInfo struct {
	name    string
	kubeTag string // kube_tag label; may be empty
}

// version returns the template's effective Kubernetes version: its kube_tag
// label if that parses, else its name (FCoS templates are named vX.Y.Z).
func (t templateInfo) version() (string, bool) {
	if _, _, _, ok := parseKubeVersion(t.kubeTag); ok {
		return t.kubeTag, true
	}
	if _, _, _, ok := parseKubeVersion(t.name); ok {
		return t.name, true
	}
	return "", false
}

// fcosTemplates keeps only the Fedora CoreOS, version-parseable templates (the
// ones conformance can create from), dropping Ubuntu ("-u" suffix) and any
// template without a usable version. It never mutates its input.
func fcosTemplates(all []templateInfo) []templateInfo {
	out := make([]templateInfo, 0, len(all))
	for _, t := range all {
		if strings.Contains(t.name, "-u") { // Ubuntu template naming (e.g. v1.29.14-u22)
			continue
		}
		if _, ok := t.version(); !ok {
			continue
		}
		out = append(out, t)
	}
	return out
}

// newestFCoSTemplate returns the highest-version FCoS template name — the base
// used for kube_tag overrides when no template pins a target version exactly.
func newestFCoSTemplate(all []templateInfo) (string, bool) {
	fc := fcosTemplates(all)
	if len(fc) == 0 {
		return "", false
	}
	sort.SliceStable(fc, func(i, j int) bool {
		vi, _ := fc[i].version()
		vj, _ := fc[j].version()
		return cmpKubeVersion(vi, vj) > 0
	})
	return fc[0].name, true
}

// resolveConformanceLegs maps each target Kubernetes version to a create recipe.
// If some FCoS template already pins that exact version (by kube_tag or name) it
// is used directly (KubeTag=""); otherwise the newest FCoS template is reused
// with the target appended as a kube_tag override. This is the "reuse current
// templates, append kube_tag for new versions" contract. Pure: no I/O.
func resolveConformanceLegs(targets []string, all []templateInfo) ([]conformanceLeg, error) {
	base, ok := newestFCoSTemplate(all)
	if !ok {
		return nil, fmt.Errorf("no Fedora CoreOS (vX.Y.Z) cluster templates found to base conformance on")
	}
	fc := fcosTemplates(all)
	legs := make([]conformanceLeg, 0, len(targets))
	for _, target := range targets {
		if _, _, _, okv := parseKubeVersion(target); !okv {
			continue
		}
		leg := conformanceLeg{Version: target, Template: base, KubeTag: target}
		for _, t := range fc {
			if v, vok := t.version(); vok && cmpKubeVersion(v, target) == 0 {
				leg = conformanceLeg{Version: target, Template: t.name, KubeTag: ""}
				break
			}
		}
		leg.Slug = strings.ReplaceAll(target, ".", "-")
		legs = append(legs, leg)
	}
	if len(legs) == 0 {
		return nil, fmt.Errorf("no valid conformance targets resolved")
	}
	return legs, nil
}

// dlK8sBase is the Kubernetes release channel host. Overridable for tests.
var dlK8sBase = "https://dl.k8s.io/release"

// fetchStableVersion returns the latest patch for a channel file such as
// "stable.txt" (newest overall) or "stable-1.35.txt" (newest 1.35 patch).
func fetchStableVersion(ctx context.Context, file string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlK8sBase+"/"+file, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %d", file, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if _, _, _, ok := parseKubeVersion(v); !ok {
		return "", fmt.Errorf("GET %s: unexpected body %q", file, v)
	}
	return v, nil
}

// newestKubeTargets returns the latest patch of the n newest Kubernetes minors,
// newest first (e.g. [v1.36.2 v1.35.7 v1.34.6 v1.33.10]). It reads stable.txt
// for the newest minor, then stable-1.<minor>.txt down the line. A minor whose
// channel file is missing is skipped (best-effort) rather than failing the whole
// resolution.
func newestKubeTargets(ctx context.Context, n int) ([]string, error) {
	latest, err := fetchStableVersion(ctx, "stable.txt")
	if err != nil {
		return nil, fmt.Errorf("resolve newest kubernetes: %w", err)
	}
	_, minor, _, _ := parseKubeVersion(latest)
	var targets []string
	for m := minor; m > minor-n && m >= 0 && len(targets) < n; m-- {
		v, ferr := fetchStableVersion(ctx, fmt.Sprintf("stable-1.%d.txt", m))
		if ferr != nil {
			continue
		}
		targets = append(targets, v)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("resolved no kubernetes target versions")
	}
	return targets, nil
}

// resolveConformanceMatrix lists the project's cluster templates, resolves the n
// newest Kubernetes versions, and emits the matrix JSON legs for the conformance
// workflow to consume as a build matrix. The JSON is written to outFile when set
// (the workflow reads that file — stdout also carries the runner's own auth/init
// logs, so a file keeps the JSON clean); otherwise it is printed to stdout.
// Human-readable progress always goes to stderr.
func (r *runner) resolveConformanceMatrix(ctx context.Context, n int, outFile string) error {
	pages, err := clustertemplates.List(r.magnum, nil).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list cluster templates: %w", err)
	}
	tmpls, err := clustertemplates.ExtractClusterTemplates(pages)
	if err != nil {
		return fmt.Errorf("extract cluster templates: %w", err)
	}
	all := make([]templateInfo, 0, len(tmpls))
	for _, t := range tmpls {
		all = append(all, templateInfo{name: t.Name, kubeTag: t.Labels["kube_tag"]})
	}

	targets, err := newestKubeTargets(ctx, n)
	if err != nil {
		return err
	}
	legs, err := resolveConformanceLegs(targets, all)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "resolved %d conformance leg(s):\n", len(legs))
	for _, l := range legs {
		via := "template pins version"
		if l.KubeTag != "" {
			via = "template " + l.Template + " + kube_tag override"
		}
		fmt.Fprintf(os.Stderr, "  %-10s -> %s\n", l.Version, via)
	}
	out, err := json.Marshal(legs)
	if err != nil {
		return err
	}
	if outFile != "" {
		if werr := os.WriteFile(outFile, append(out, '\n'), 0o644); werr != nil {
			return fmt.Errorf("write matrix to %s: %w", outFile, werr)
		}
		fmt.Fprintf(os.Stderr, "wrote conformance matrix to %s\n", outFile)
		return nil
	}
	fmt.Println(string(out))
	return nil
}

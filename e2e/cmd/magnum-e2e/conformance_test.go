package main

import (
	"encoding/json"
	"testing"
)

func TestParseKubeVersion(t *testing.T) {
	cases := []struct {
		in            string
		maj, min, pat int
		ok            bool
	}{
		{"v1.36.2", 1, 36, 2, true},
		{"v1.33.10", 1, 33, 10, true},
		{" v1.30.0 ", 1, 30, 0, true},
		{"v1.29.14-u22", 0, 0, 0, false}, // Ubuntu suffix
		{"1.36.2", 0, 0, 0, false},       // missing v
		{"v1.36", 0, 0, 0, false},        // no patch
		{"latest", 0, 0, 0, false},
		{"", 0, 0, 0, false},
	}
	for _, c := range cases {
		maj, min, pat, ok := parseKubeVersion(c.in)
		if ok != c.ok || (ok && (maj != c.maj || min != c.min || pat != c.pat)) {
			t.Errorf("parseKubeVersion(%q) = (%d,%d,%d,%t), want (%d,%d,%d,%t)",
				c.in, maj, min, pat, ok, c.maj, c.min, c.pat, c.ok)
		}
	}
}

func TestCmpKubeVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.36.2", "v1.36.2", 0},
		{"v1.36.2", "v1.35.9", 1},
		{"v1.33.10", "v1.33.2", 1}, // numeric, not lexical (10 > 2)
		{"v1.34.0", "v1.34.1", -1},
		{"v2.0.0", "v1.99.99", 1},
		{"bad", "v1.0.0", -1}, // unparseable sorts lowest
		{"v1.0.0", "bad", 1},
	}
	for _, c := range cases {
		if got := cmpKubeVersion(c.a, c.b); got != c.want {
			t.Errorf("cmpKubeVersion(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNewestFCoSTemplate(t *testing.T) {
	all := []templateInfo{
		{name: "v1.33.10"},
		{name: "v1.36.2"},
		{name: "v1.29.14-u22", kubeTag: "v1.29.14"}, // Ubuntu — excluded
		{name: "v1.35.3"},
		{name: "some-random-template"}, // no version — excluded
	}
	got, ok := newestFCoSTemplate(all)
	if !ok || got != "v1.36.2" {
		t.Fatalf("newestFCoSTemplate = (%q,%t), want (v1.36.2,true)", got, ok)
	}

	if _, ok := newestFCoSTemplate([]templateInfo{{name: "v1.29.14-u22"}, {name: "junk"}}); ok {
		t.Fatal("expected no FCoS base when only Ubuntu/invalid templates exist")
	}
}

func TestNewestFCoSTemplatePrefersKubeTag(t *testing.T) {
	// A generically-named template whose kube_tag is newest must win, and its
	// NAME (not the kube_tag) is what create uses.
	all := []templateInfo{
		{name: "v1.35.3"},
		{name: "k8s-base", kubeTag: "v1.37.0"},
	}
	got, ok := newestFCoSTemplate(all)
	if !ok || got != "k8s-base" {
		t.Fatalf("newestFCoSTemplate = (%q,%t), want (k8s-base,true)", got, ok)
	}
}

func TestResolveConformanceLegs(t *testing.T) {
	all := []templateInfo{
		{name: "v1.36.2"},
		{name: "v1.35.3"},
		{name: "v1.34.6"},
		{name: "v1.33.10"},
		{name: "v1.29.14-u22"},
	}
	// Targets: two exactly match a pinned template, two are newer patches with no
	// pinned template → must reuse the newest base (v1.36.2) + kube_tag override.
	targets := []string{"v1.36.5", "v1.35.3", "v1.34.6", "v1.33.99"}
	legs, err := resolveConformanceLegs(targets, all)
	if err != nil {
		t.Fatalf("resolveConformanceLegs: %v", err)
	}
	if len(legs) != 4 {
		t.Fatalf("want 4 legs, got %d: %+v", len(legs), legs)
	}
	want := []conformanceLeg{
		{Version: "v1.36.5", Template: "v1.36.2", KubeTag: "v1.36.5", Slug: "v1-36-5"},    // new patch → override on newest base
		{Version: "v1.35.3", Template: "v1.35.3", KubeTag: "", Slug: "v1-35-3"},           // pinned template exists
		{Version: "v1.34.6", Template: "v1.34.6", KubeTag: "", Slug: "v1-34-6"},           // pinned template exists
		{Version: "v1.33.99", Template: "v1.36.2", KubeTag: "v1.33.99", Slug: "v1-33-99"}, // new patch → override
	}
	for i := range want {
		if legs[i] != want[i] {
			t.Errorf("leg[%d] = %+v, want %+v", i, legs[i], want[i])
		}
	}
}

func TestResolveConformanceLegsMatchesByKubeTag(t *testing.T) {
	// A template named generically but pinning the target via kube_tag counts as
	// an exact match (no override needed).
	all := []templateInfo{
		{name: "base-newest", kubeTag: "v1.36.2"},
		{name: "conformance-135", kubeTag: "v1.35.3"},
	}
	legs, err := resolveConformanceLegs([]string{"v1.35.3"}, all)
	if err != nil {
		t.Fatalf("resolveConformanceLegs: %v", err)
	}
	if legs[0] != (conformanceLeg{Version: "v1.35.3", Template: "conformance-135", KubeTag: "", Slug: "v1-35-3"}) {
		t.Fatalf("kube_tag match not used: %+v", legs[0])
	}
}

func TestResolveConformanceLegsNoBase(t *testing.T) {
	if _, err := resolveConformanceLegs([]string{"v1.36.2"}, []templateInfo{{name: "v1.29.14-u22"}}); err == nil {
		t.Fatal("expected error when no FCoS base template exists")
	}
}

func TestConformanceLegJSONMatrixShape(t *testing.T) {
	// The workflow does fromJSON on this; assert the field names/shape are stable.
	legs := []conformanceLeg{{Version: "v1.36.2", Template: "v1.36.2", KubeTag: "", Slug: "v1-36-2"}}
	b, err := json.Marshal(legs)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `[{"version":"v1.36.2","template":"v1.36.2","kubeTag":"","slug":"v1-36-2"}]` {
		t.Fatalf("unexpected matrix JSON: %s", got)
	}
}

package config

import "testing"

func TestParseOSReleaseID(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"ubuntu", "NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\"22.04\"\n", "ubuntu"},
		{"fedora-coreos", "NAME=\"Fedora Linux\"\nID=fedora\nVARIANT_ID=coreos\n", "fedora"},
		{"double-quoted", "PRETTY_NAME=\"x\"\nID=\"debian\"\n", "debian"},
		{"single-quoted", "ID='ubuntu'\n", "ubuntu"},
		{"uppercase-value-lowered", "ID=Ubuntu\n", "ubuntu"},
		{"missing", "NAME=\"Whatever\"\n", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseOSReleaseID(c.content); got != c.want {
				t.Fatalf("parseOSReleaseID(%q) = %q, want %q", c.content, got, c.want)
			}
		})
	}
}

func TestDistroPredicates(t *testing.T) {
	cases := []struct {
		distro     string
		wantUbuntu bool
		wantFCoS   bool
	}{
		{"ubuntu", true, false},
		{"debian", true, false},
		{"fedora", false, true},
		{"fedora-coreos", false, true},
		{"rhcos", false, true},
		{"", false, true}, // unknown defaults to FCoS behavior (back-compat)
	}
	for _, c := range cases {
		cfg := Config{Distro: c.distro}
		if got := cfg.IsUbuntu(); got != c.wantUbuntu {
			t.Errorf("Distro=%q IsUbuntu()=%v, want %v", c.distro, got, c.wantUbuntu)
		}
		if got := cfg.IsFCoS(); got != c.wantFCoS {
			t.Errorf("Distro=%q IsFCoS()=%v, want %v", c.distro, got, c.wantFCoS)
		}
	}
}

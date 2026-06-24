package reconcile

import (
	"testing"
	"time"
)

func TestResolveRefreshTimeout(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want time.Duration
	}{
		{name: "unset uses default", set: false, want: defaultRefreshTimeout},
		{name: "empty uses default", set: true, val: "", want: defaultRefreshTimeout},
		{name: "valid seconds", set: true, val: "120", want: 120 * time.Second},
		{name: "zero disables", set: true, val: "0", want: 0},
		{name: "negative uses default", set: true, val: "-5", want: defaultRefreshTimeout},
		{name: "garbage uses default", set: true, val: "abc", want: defaultRefreshTimeout},
		{name: "whitespace trimmed", set: true, val: "  90  ", want: 90 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(refreshTimeoutEnv, tc.val)
			} else {
				// t.Setenv guarantees restore; set then unset to isolate.
				t.Setenv(refreshTimeoutEnv, "")
			}
			if got := resolveRefreshTimeout(); got != tc.want {
				t.Fatalf("resolveRefreshTimeout()=%s want %s", got, tc.want)
			}
		})
	}
}

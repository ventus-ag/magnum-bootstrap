package moduleapi

import (
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

func TestDisruptiveServiceCycleNeeded(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		req  Request
		want bool
	}{
		{
			// Fresh node (create or scale-add): nothing converged before, nothing
			// to drain — even though a desired tag is set.
			name: "fresh node never drains",
			cfg:  config.Config{Shared: config.SharedConfig{KubeTag: "v1.29.0"}},
			req:  Request{},
			want: false,
		},
		{
			// Converged node moving to a new version → drain/restart cycle.
			name: "version change triggers disruption",
			cfg:  config.Config{Shared: config.SharedConfig{KubeTag: "v1.29.0"}},
			req:  Request{PreviousSuccessfulGeneration: "create:v1.28.0", PreviousKubeTag: "v1.28.0"},
			want: true,
		},
		{
			// Same tag (periodic reconcile / re-apply): no version change → no drain.
			name: "same tag stays non disruptive",
			cfg:  config.Config{Shared: config.SharedConfig{KubeTag: "v1.29.0"}},
			req:  Request{PreviousSuccessfulGeneration: "create:v1.29.0", PreviousKubeTag: "v1.29.0"},
			want: false,
		},
		{
			// Resize touches an existing node at the SAME version — it keeps
			// running, so no drain (improvement over the old IS_RESIZE behavior
			// which drained existing nodes on every resize).
			name: "resize without version change does not drain existing node",
			cfg:  config.Config{Shared: config.SharedConfig{KubeTag: "v1.29.0"}},
			req:  Request{PreviousSuccessfulGeneration: "create:v1.29.0", PreviousKubeTag: "v1.29.0"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DisruptiveServiceCycleNeeded(tt.cfg, tt.req); got != tt.want {
				t.Fatalf("DisruptiveServiceCycleNeeded() = %t, want %t", got, tt.want)
			}
		})
	}
}

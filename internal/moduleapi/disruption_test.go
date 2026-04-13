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
			name: "no disruptive flags",
			cfg:  config.Config{Shared: config.SharedConfig{KubeTag: "v1.29.0"}},
			req:  Request{PreviousSuccessfulGeneration: "create:v1.28.0"},
			want: false,
		},
		{
			name: "upgrade without previous success skips disruption",
			cfg:  config.Config{Shared: config.SharedConfig{IsUpgrade: true, KubeTag: "v1.29.0"}},
			req:  Request{},
			want: false,
		},
		{
			name: "upgrade with kube tag change triggers disruption",
			cfg:  config.Config{Shared: config.SharedConfig{IsUpgrade: true, KubeTag: "v1.29.0"}},
			req:  Request{PreviousSuccessfulGeneration: "create:v1.28.0", PreviousKubeTag: "v1.28.0"},
			want: true,
		},
		{
			name: "same tag upgrade stays non disruptive",
			cfg:  config.Config{Shared: config.SharedConfig{IsUpgrade: true, KubeTag: "v1.29.0"}},
			req:  Request{PreviousSuccessfulGeneration: "create:v1.29.0", PreviousKubeTag: "v1.29.0"},
			want: false,
		},
		{
			name: "stale upgrade flag does not retrigger disruption",
			cfg:  config.Config{Shared: config.SharedConfig{IsUpgrade: true, KubeTag: "v1.29.0"}},
			req:  Request{PreviousSuccessfulGeneration: "upgrade:v1.29.0", PreviousKubeTag: "v1.29.0"},
			want: false,
		},
		{
			name: "resize generation change triggers disruption",
			cfg:  config.Config{Shared: config.SharedConfig{IsResize: true, KubeTag: "v1.29.0"}},
			req:  Request{PreviousSuccessfulGeneration: "create:v1.29.0"},
			want: true,
		},
		{
			name: "stale resize flag does not retrigger disruption",
			cfg:  config.Config{Shared: config.SharedConfig{IsResize: true, KubeTag: "v1.29.0"}},
			req:  Request{PreviousSuccessfulGeneration: "resize:v1.29.0"},
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

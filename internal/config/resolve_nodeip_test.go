package config

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchMetadataNodeIPOnce(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    string
		wantErr bool
	}{
		{"valid ip", 200, "10.0.0.5\n", "10.0.0.5", false},
		{"error status with html body", 404, "<html>not found</html>", "", true},
		{"200 with garbage body", 200, "<html>captive portal</html>", "", true},
		{"empty body", 200, "", "", true},
		{"ipv6", 200, "fd00::1", "fd00::1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			orig := metadataNodeIPURL
			metadataNodeIPURL = srv.URL
			defer func() { metadataNodeIPURL = orig }()

			got, err := fetchMetadataNodeIPOnce(&http.Client{Timeout: time.Second})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestResolveNodeIPPrefersConfig(t *testing.T) {
	var c Config
	c.Shared.KubeNodeIP = "192.168.1.7"
	if got := c.ResolveNodeIP(); got != "192.168.1.7" {
		t.Fatalf("got %q", got)
	}
}

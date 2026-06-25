package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestContainerRuntimeHostDockerNormalization guards the host-docker→containerd
// migration. Old clusters carry CONTAINER_RUNTIME=host-docker, which is the
// dockershim-era "use the FCoS-baked moby+containerd" mode. dockershim was
// removed in Kubernetes 1.24 and the baked containerd (1.5) only serves CRI
// v1alpha2, so kubelet >= 1.24 cannot start against it. For >= 1.24 the label
// is normalized to "containerd" so the container-runtime module installs and
// owns a modern containerd. Below 1.24 the legacy behavior is preserved.
func TestContainerRuntimeHostDockerNormalization(t *testing.T) {
	cases := []struct {
		name    string
		runtime string
		kubeTag string
		want    string
	}{
		{"host-docker on 1.28 -> containerd", "host-docker", "v1.28.4", "containerd"},
		{"host-docker on 1.24 -> containerd", "host-docker", "v1.24.0", "containerd"},
		{"host-docker on 1.23 stays legacy", "host-docker", "v1.23.17", "host-docker"},
		{"containerd unchanged", "containerd", "v1.28.4", "containerd"},
		{"docker (cri-dockerd) unchanged", "docker", "v1.28.4", "docker"},
		{"case-insensitive host-docker", "Host-Docker", "v1.28.4", "containerd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "heat-params")
			body := "CONTAINER_RUNTIME=\"" + tc.runtime + "\"\nKUBE_TAG=\"" + tc.kubeTag + "\"\n"
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatalf("write heat-params: %v", err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Shared.ContainerRuntime != tc.want {
				t.Errorf("ContainerRuntime = %q, want %q", cfg.Shared.ContainerRuntime, tc.want)
			}
		})
	}
}

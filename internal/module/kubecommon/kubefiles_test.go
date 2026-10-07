package kubecommon

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

func encodeKubeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestDecodeKubeFiles(t *testing.T) {
	files, err := DecodeKubeFiles(encodeKubeFiles(t, map[string]string{"oidc_ca": "-----BEGIN CERTIFICATE-----\nAA==\n"}))
	if err != nil || files["oidc_ca"] != "-----BEGIN CERTIFICATE-----\nAA==\n" {
		t.Fatalf("DecodeKubeFiles = %v, %v", files, err)
	}
	if files, err := DecodeKubeFiles(" "); err != nil || files != nil {
		t.Fatalf("empty must decode to nothing, got %v %v", files, err)
	}
	for _, bad := range []string{"not base64!", base64.StdEncoding.EncodeToString([]byte("[1]")),
		encodeKubeFiles(t, map[string]string{"../etc/passwd": "x"})} {
		if _, err := DecodeKubeFiles(bad); err == nil {
			t.Errorf("DecodeKubeFiles(%q) must fail", bad)
		}
	}
}

func TestEnsureKubeFilesConvergesDirectory(t *testing.T) {
	dir := t.TempDir()
	restore := kubeFilesDir
	kubeFilesDir = dir
	defer func() { kubeFilesDir = restore }()
	executor := host.NewExecutor(true, nil)

	if _, err := EnsureKubeFiles(executor, map[string]string{"a": "one", "b": "two"}); err != nil {
		t.Fatal(err)
	}
	// Second pass with the same set is a no-op; dropping a label removes its file.
	if changes, err := EnsureKubeFiles(executor, map[string]string{"a": "one", "b": "two"}); err != nil || len(changes) != 0 {
		t.Fatalf("unchanged set must be a no-op, got %v %v", changes, err)
	}
	// A file placed by hand is never ours to delete.
	if err := os.WriteFile(filepath.Join(dir, "manual"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureKubeFiles(executor, map[string]string{"a": "uno"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a"))
	if string(got) != "uno" {
		t.Fatalf("a = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "b")); !os.IsNotExist(err) {
		t.Fatalf("b must be removed with its label, stat err = %v", err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "a")); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	if _, err := EnsureKubeFiles(executor, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Fatal("removing the last label must remove its file")
	}
	if _, err := os.Stat(filepath.Join(dir, "manual")); err != nil {
		t.Fatalf("a hand-placed file was deleted: %v", err)
	}
}

func TestMissingFileFlags(t *testing.T) {
	present := map[string]bool{"/etc/kubernetes/files/oidc_ca": true}
	exists := func(p string) bool { return present[p] }
	for _, tc := range []struct {
		opts string
		want []string
	}{
		{"--oidc-issuer-url=https://kc --oidc-ca-file=/etc/kubernetes/files/oidc_ca", nil},
		{"--oidc-ca-file=/etc/kubernetes/certs/oidc-custom-ca.pem --oidc-client-id=x", []string{"--oidc-ca-file=/etc/kubernetes/certs/oidc-custom-ca.pem"}},
		{"--audit-policy-file /x/policy.yaml --v=2", []string{"--audit-policy-file=/x/policy.yaml"}},
		{`--authentication-config="/x/authn.yaml"`, []string{"--authentication-config=/x/authn.yaml"}},
		{"--audit-log-path=/var/log/audit.log --log-file=/x.log --lock-file=/run/k.lock --pod-manifest-path=/nope", nil},
		{"--config=relative.yaml --tls-cert-file", nil},
		{"--enable-admission-plugins=NodeRestriction --max-pods=50", nil},
	} {
		if got := MissingFileFlags(tc.opts, exists); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("MissingFileFlags(%q) = %v, want %v", tc.opts, got, tc.want)
		}
	}
}

func TestCheckOptionFilesCountsProvisionedPaths(t *testing.T) {
	provisioned := map[string]bool{"/etc/kubernetes/files/oidc_ca": true}
	if err := CheckOptionFiles(provisioned, map[string]string{"kubeapi_options": "--oidc-ca-file=/etc/kubernetes/files/oidc_ca"}); err != nil {
		t.Fatalf("a file provisioned in this run must count as present: %v", err)
	}
	if err := CheckOptionFiles(nil, map[string]string{"kubeapi_options": "--oidc-ca-file=/nonexistent/ca.pem"}); err == nil {
		t.Fatal("a missing file must fail before the args are written")
	}
}

func TestRestartUnits(t *testing.T) {
	units := []UnitOptions{
		{Unit: "kube-apiserver", Options: "--audit-policy-file=/etc/kubernetes/files/policy"},
		{Unit: "kubelet", Options: "--max-pods=50"},
		{Unit: "kube-proxy"},
	}
	kubeFile := host.Change{Action: host.ActionCreate, Path: "/etc/kubernetes/files/policy"}
	other := host.Change{Action: host.ActionReplace, Path: "/etc/kubernetes/apiserver"}
	for _, tc := range []struct {
		name    string
		changes []host.Change
		want    []string
	}{
		{"none", nil, nil},
		{"kube file only", []host.Change{kubeFile}, []string{"kube-apiserver"}},
		{"config change", []host.Change{kubeFile, other}, []string{"kube-apiserver", "kubelet", "kube-proxy"}},
		{"no path", []host.Change{{Action: host.ActionUpdate}}, []string{"kube-apiserver", "kubelet", "kube-proxy"}},
	} {
		if got := RestartUnits(tc.changes, units); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: RestartUnits = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func BenchmarkMissingFileFlags(b *testing.B) {
	opts := "--oidc-issuer-url=https://keycloak.example/realms/apps --oidc-username-claim=email --oidc-client-id=headlamp --oidc-ca-file=/etc/kubernetes/files/oidc_ca --audit-log-path=/var/log/a.log"
	exists := func(string) bool { return true }
	for b.Loop() {
		MissingFileFlags(opts, exists)
	}
}

func BenchmarkRestartUnits(b *testing.B) {
	units := []UnitOptions{
		{Unit: "kube-apiserver", Options: "--oidc-issuer-url=https://kc --oidc-ca-file=/etc/kubernetes/files/oidc_ca"},
		{Unit: "kube-controller-manager"}, {Unit: "kube-scheduler"}, {Unit: "kubelet"}, {Unit: "kube-proxy"},
	}
	changes := []host.Change{{Path: "/etc/kubernetes/files/oidc_ca"}, {Path: "/etc/kubernetes/files/.magnum-managed"}}
	for b.Loop() {
		RestartUnits(changes, units)
	}
}

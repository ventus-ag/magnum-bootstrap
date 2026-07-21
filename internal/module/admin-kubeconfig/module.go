package adminkubeconfig

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState

	TargetPath pulumi.StringOutput `pulumi:"targetPath"`
	HomeCopy   pulumi.StringOutput `pulumi:"homeCopy"`
	Content    pulumi.StringOutput `pulumi:"content"`
}

func (Module) PhaseID() string {
	return "admin-kubeconfig"
}
func (Module) Dependencies() []string { return []string{"master-certificates", "worker-certificates"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	content, err := BuildContent(cfg)
	if err != nil {
		return moduleapi.Result{}, err
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	changes := make([]host.Change, 0)

	for _, dir := range []string{"/etc/kubernetes", "/root/.kube"} {
		result, err := (hostresource.DirectorySpec{Path: dir, Mode: 0o755}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, result.Changes...)
	}

	for _, file := range []struct {
		path    string
		content string
		mode    os.FileMode
	}{
		{path: "/etc/kubernetes/admin.conf", content: content, mode: 0o600},
		{path: "/root/.kube/config", content: content, mode: 0o600},
	} {
		result, err := (hostresource.FileSpec{Path: file.path, Content: []byte(file.content), Mode: file.mode}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, result.Changes...)
	}

	// Copy kubeconfig to non-root user home directories so operators can
	// use kubectl without sudo.  Fedora CoreOS → core, Ubuntu → ubuntu.
	for _, user := range []string{"core", "ubuntu"} {
		homeDir := "/home/" + user
		if _, err := os.Stat(homeDir); err != nil {
			continue
		}
		kubeDir := homeDir + "/.kube"
		dirResult, err := (hostresource.DirectorySpec{Path: kubeDir, Mode: 0o755}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, dirResult.Changes...)
		fileResult, err := (hostresource.FileSpec{Path: kubeDir + "/config", Content: []byte(content), Mode: 0o600}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, fileResult.Changes...)
		_ = executor.Run("chown", "-R", user+":"+user, kubeDir)
	}

	exportResult, err := (hostresource.ExportSpec{Path: "/etc/bashrc", VarName: "KUBECONFIG", Value: "/etc/kubernetes/admin.conf", Mode: 0o644}).Apply(executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, exportResult.Changes...)

	// Workers additionally get the node-manager kubeconfig: a scoped
	// credential for node metadata (labels/taints) reconciliation that is not
	// subject to NodeRestriction like the kubelet identity above.
	if nmContent, ok := BuildNodeManagerContent(cfg); ok {
		result, err := (hostresource.FileSpec{Path: "/etc/kubernetes/node-manager.conf", Content: []byte(nmContent), Mode: 0o600}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, result.Changes...)
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{
			"targetPath": "/etc/kubernetes/admin.conf",
			"homeCopy":   "/root/.kube/config",
			"content":    content,
		},
	}, nil
}

// Destroy removes admin kubeconfig files.
func (Module) Destroy(_ context.Context, _ config.Config, req moduleapi.Request) error {
	if req.Logger != nil {
		req.Logger.Infof("admin-kubeconfig destroy: removing kubeconfig files")
	}
	_ = os.Remove("/etc/kubernetes/admin.conf")
	_ = os.Remove("/etc/kubernetes/node-manager.conf")
	_ = os.Remove("/root/.kube/config")
	for _, user := range []string{"core", "ubuntu"} {
		_ = os.RemoveAll("/home/" + user + "/.kube")
	}

	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:WriteAdminKubeconfig", name, res, opts...); err != nil {
		return nil, err
	}

	content, err := BuildContent(cfg)
	if err != nil {
		return nil, err
	}
	childOpts := hostresource.ChildResourceOptions(res, opts...)
	dirResources := map[string]pulumi.Resource{}
	var adminConfRes pulumi.Resource
	for _, dir := range []string{"/etc/kubernetes", "/root/.kube"} {
		resDir, err := hostsdk.RegisterDirectorySpec(ctx, name+"-dir-"+strings.ReplaceAll(strings.Trim(dir, "/"), "/", "-"), hostresource.DirectorySpec{Path: dir, Mode: 0o755}, childOpts...)
		if err != nil {
			return nil, err
		}
		dirResources[dir] = resDir
	}
	for _, file := range []struct {
		name string
		path string
		mode os.FileMode
	}{
		{name: "admin-conf", path: "/etc/kubernetes/admin.conf", mode: 0o600},
		{name: "root-kubeconfig", path: "/root/.kube/config", mode: 0o600},
	} {
		parentDir := filepath.Dir(file.path)
		fileOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, dirResources[parentDir])
		fileRes, err := hostsdk.RegisterFileSpec(ctx, name+"-"+file.name, hostresource.FileSpec{Path: file.path, Content: []byte(content), Mode: file.mode}, fileOpts...)
		if err != nil {
			return nil, err
		}
		if file.name == "admin-conf" {
			adminConfRes = fileRes
		}
	}
	for _, user := range []string{"core", "ubuntu"} {
		homeDir := "/home/" + user
		if _, err := os.Stat(homeDir); err != nil {
			continue
		}
		kubeDir := homeDir + "/.kube"
		userDirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-"+user+"-dir", hostresource.DirectorySpec{Path: kubeDir, Mode: 0o755}, childOpts...)
		if err != nil {
			return nil, err
		}
		userFileOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, userDirRes, adminConfRes)
		if _, err := hostsdk.RegisterFileSpec(ctx, name+"-"+user+"-config", hostresource.FileSpec{Path: kubeDir + "/config", Content: []byte(content), Mode: 0o600}, userFileOpts...); err != nil {
			return nil, err
		}
	}
	exportOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, adminConfRes)
	if _, err := hostsdk.RegisterExportSpec(ctx, name+"-bashrc-export", hostresource.ExportSpec{Path: "/etc/bashrc", VarName: "KUBECONFIG", Value: "/etc/kubernetes/admin.conf", Mode: 0o644}, exportOpts...); err != nil {
		return nil, err
	}

	if nmContent, ok := BuildNodeManagerContent(cfg); ok {
		nmOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, dirResources["/etc/kubernetes"])
		if _, err := hostsdk.RegisterFileSpec(ctx, name+"-node-manager-conf", hostresource.FileSpec{Path: "/etc/kubernetes/node-manager.conf", Content: []byte(nmContent), Mode: 0o600}, nmOpts...); err != nil {
			return nil, err
		}
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"targetPath": pulumi.String("/etc/kubernetes/admin.conf"),
		"homeCopy":   pulumi.String("/root/.kube/config"),
		"content":    pulumi.String(content),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

// BuildContent renders the admin kubeconfig for this node's role. It is the
// single source of admin.conf content — ca-rotation writes the same file
// after installing rotated certs and must render identical bytes, or every
// rotation is followed by a spurious admin-kubeconfig rewrite on the next
// reconcile.
func BuildContent(cfg config.Config) (string, error) {
	const certDir = "/etc/kubernetes/certs"

	if cfg.Role() == config.RoleMaster {
		caData, err := readBase64(certDir + "/ca.crt")
		if err != nil {
			return "", err
		}
		adminCert, err := readBase64(certDir + "/admin.crt")
		if err != nil {
			return "", err
		}
		adminKey, err := readBase64(certDir + "/admin.key")
		if err != nil {
			return "", err
		}
		apiPort := cfg.Shared.KubeAPIPort
		if apiPort == 0 {
			apiPort = 6443
		}
		protocol := "https"
		if cfg.Shared.TLSDisabled {
			protocol = "http"
		}
		return strings.TrimSpace(fmt.Sprintf(`
apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: %s
    server: %s://127.0.0.1:%d
  name: %s
contexts:
- context:
    cluster: %s
    user: admin
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: admin
  user:
    as-user-extra: {}
    client-certificate-data: %s
    client-key-data: %s
`, caData, protocol, apiPort, cfg.Shared.ClusterUUID, cfg.Shared.ClusterUUID, adminCert, adminKey)) + "\n", nil
	}

	if cfg.Worker == nil {
		return "", fmt.Errorf("worker kubeconfig requested without worker configuration")
	}
	if _, err := os.Stat(certDir + "/kubelet.crt"); err != nil {
		return "", fmt.Errorf("required worker certificate not found: %w", err)
	}
	if _, err := os.Stat(certDir + "/kubelet.key"); err != nil {
		return "", fmt.Errorf("required worker key not found: %w", err)
	}

	protocol := "https"
	if cfg.Shared.TLSDisabled {
		protocol = "http"
	}
	server := fmt.Sprintf("%s://%s:%d", protocol, cfg.Worker.KubeMasterIP, cfg.Shared.KubeAPIPort)
	return strings.TrimSpace(fmt.Sprintf(`
apiVersion: v1
clusters:
- cluster:
    certificate-authority: %s/ca.crt
    server: %s
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: system:node:%s
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: system:node:%s
  user:
    as-user-extra: {}
    client-certificate: %s/kubelet.crt
    client-key: %s/kubelet.key
`, certDir, server, cfg.Shared.InstanceName, cfg.Shared.InstanceName, certDir, certDir)) + "\n", nil
}

func readBase64(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// BuildNodeManagerContent renders the worker node-manager kubeconfig (scoped
// node metadata credential). Returns ok=false on masters, TLS-disabled
// clusters, or while the node-manager cert has not been issued yet — callers
// simply skip the file and converge on a later run.
func BuildNodeManagerContent(cfg config.Config) (string, bool) {
	const certDir = "/etc/kubernetes/certs"
	if cfg.Role() == config.RoleMaster || cfg.Worker == nil || cfg.Shared.TLSDisabled {
		return "", false
	}
	if _, err := os.Stat(certDir + "/node-manager.crt"); err != nil {
		return "", false
	}
	if _, err := os.Stat(certDir + "/node-manager.key"); err != nil {
		return "", false
	}
	user := "magnum:node-manager:" + cfg.Shared.InstanceName
	server := fmt.Sprintf("https://%s:%d", cfg.Worker.KubeMasterIP, cfg.Shared.KubeAPIPort)
	return strings.TrimSpace(fmt.Sprintf(`
apiVersion: v1
clusters:
- cluster:
    certificate-authority: %s/ca.crt
    server: %s
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: %s
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: %s
  user:
    as-user-extra: {}
    client-certificate: %s/node-manager.crt
    client-key: %s/node-manager.key
`, certDir, server, user, user, certDir, certDir)) + "\n", true
}

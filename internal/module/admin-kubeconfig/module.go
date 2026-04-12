package adminkubeconfig

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
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
	content, err := buildContent(cfg)
	if err != nil {
		return moduleapi.Result{}, err
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	changes := make([]host.Change, 0)

	for _, dir := range []string{"/etc/kubernetes", "/root/.kube"} {
		change, err := executor.EnsureDir(dir, 0o755)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	for _, file := range []struct {
		path    string
		content string
		mode    os.FileMode
	}{
		{path: "/etc/kubernetes/admin.conf", content: content, mode: 0o600},
		{path: "/root/.kube/config", content: content, mode: 0o600},
	} {
		change, err := executor.EnsureFile(file.path, []byte(file.content), file.mode)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	// Copy kubeconfig to non-root user home directories so operators can
	// use kubectl without sudo.  Fedora CoreOS → core, Ubuntu → ubuntu.
	for _, user := range []string{"core", "ubuntu"} {
		homeDir := "/home/" + user
		if _, err := os.Stat(homeDir); err != nil {
			continue
		}
		kubeDir := homeDir + "/.kube"
		change, err := executor.EnsureDir(kubeDir, 0o755)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
		change, err = executor.EnsureFile(kubeDir+"/config", []byte(content), 0o600)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
		_ = executor.Run("chown", "-R", user+":"+user, kubeDir)
	}

	change, err := executor.UpsertExport("/etc/bashrc", "KUBECONFIG", "/etc/kubernetes/admin.conf", 0o644)
	if err != nil {
		return moduleapi.Result{}, err
	}
	if change != nil {
		changes = append(changes, *change)
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

	content, err := buildContent(cfg)
	if err != nil {
		return nil, err
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

func buildContent(cfg config.Config) (string, error) {
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
		return strings.TrimSpace(fmt.Sprintf(`
apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: %s
    server: https://127.0.0.1:%d
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
`, caData, cfg.Shared.KubeAPIPort, cfg.Shared.ClusterUUID, cfg.Shared.ClusterUUID, adminCert, adminKey)) + "\n", nil
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

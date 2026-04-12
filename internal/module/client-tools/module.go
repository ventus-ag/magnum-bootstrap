package clienttools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState

	KubeletURL pulumi.StringOutput `pulumi:"kubeletUrl"`
	KubectlURL pulumi.StringOutput `pulumi:"kubectlUrl"`
	TargetDir  pulumi.StringOutput `pulumi:"targetDir"`
}

type installState struct {
	KubeTag           string `json:"kubeTag"`
	Arch              string `json:"arch"`
	KubeletURL        string `json:"kubeletUrl"`
	KubectlURL        string `json:"kubectlUrl"`
	KubeletSHA256     string `json:"kubeletSha256"`
	KubectlSHA256     string `json:"kubectlSha256"`
	KubectlCopySHA256 string `json:"kubectlCopySha256"`
}

func (Module) PhaseID() string {
	return "client-tools"
}
func (Module) Dependencies() []string { return []string{"ca-rotation"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	executor := host.NewExecutor(req.Apply, req.Logger)
	changes := make([]host.Change, 0)

	for _, dir := range []string{"/srv/magnum/bin", "/srv/magnum/k8s", moduleStateDir(req)} {
		change, err := executor.EnsureDir(dir, 0o755)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	for _, line := range []string{
		"export PATH=/srv/magnum/bin:$PATH",
		"export HISTCONTROL=ignoredups",
	} {
		change, err := executor.EnsureLine("/root/.bashrc", line, 0o644)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	desired := installState{
		KubeTag:    cfg.Shared.KubeTag,
		Arch:       cfg.Shared.Arch,
		KubeletURL: kubeletURL(cfg),
		KubectlURL: kubectlURL(cfg),
	}

	installed, err := loadState(moduleStateFile(req))
	if err != nil {
		return moduleapi.Result{}, err
	}

	needsKubelet := binaryNeedsReconcile("/usr/local/bin/kubelet", desired.KubeletURL, installed.KubeletURL, installed.KubeletSHA256)
	needsKubectl := binaryNeedsReconcile("/usr/local/bin/kubectl", desired.KubectlURL, installed.KubectlURL, installed.KubectlSHA256)
	needsKubectlCopy := binaryNeedsReconcile("/srv/magnum/bin/kubectl", desired.KubectlURL, installed.KubectlURL, installed.KubectlCopySHA256) || needsKubectl

	if req.Apply {
		if needsKubelet {
			download, err := executor.DownloadFileWithRetry(ctx, desired.KubeletURL, "/usr/local/bin/kubelet", 0o755, 5)
			if err != nil {
				return moduleapi.Result{}, err
			}
			if download.Change != nil {
				changes = append(changes, *download.Change)
			}
			desired.KubeletSHA256 = download.Checksum
		} else {
			desired.KubeletSHA256, _ = host.FileSHA256("/usr/local/bin/kubelet")
		}

		if needsKubectl {
			download, err := executor.DownloadFileWithRetry(ctx, desired.KubectlURL, "/usr/local/bin/kubectl", 0o755, 5)
			if err != nil {
				return moduleapi.Result{}, err
			}
			if download.Change != nil {
				changes = append(changes, *download.Change)
			}
			desired.KubectlSHA256 = download.Checksum
		} else {
			desired.KubectlSHA256, _ = host.FileSHA256("/usr/local/bin/kubectl")
		}

		if needsKubectlCopy {
			change, err := executor.EnsureCopy("/usr/local/bin/kubectl", "/srv/magnum/bin/kubectl", 0o755)
			if err != nil {
				return moduleapi.Result{}, err
			}
			if change != nil {
				changes = append(changes, *change)
			}
		}
		desired.KubectlCopySHA256, _ = host.FileSHA256("/srv/magnum/bin/kubectl")

		if cfg.Shared.SELinuxMode == "enforcing" && (needsKubelet || needsKubectl || needsKubectlCopy) {
			_ = executor.Run("chcon", "system_u:object_r:bin_t:s0", "/usr/local/bin/kubelet", "/usr/local/bin/kubectl", "/srv/magnum/bin/kubectl")
		}
		if err := saveState(moduleStateFile(req), desired); err != nil {
			return moduleapi.Result{}, err
		}
	} else {
		if change := plannedBinaryChange("/usr/local/bin/kubelet", desired.KubeletURL, needsKubelet, fmt.Sprintf("download kubelet %s", cfg.Shared.KubeTag)); change != nil {
			changes = append(changes, *change)
		}
		if change := plannedBinaryChange("/usr/local/bin/kubectl", desired.KubectlURL, needsKubectl, fmt.Sprintf("download kubectl %s", cfg.Shared.KubeTag)); change != nil {
			changes = append(changes, *change)
		}
		if needsKubectlCopy {
			change, err := plannedCopyChange("/usr/local/bin/kubectl", "/srv/magnum/bin/kubectl", 0o755)
			if err != nil {
				return moduleapi.Result{}, err
			}
			if change != nil {
				changes = append(changes, *change)
			}
		}
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{
			"kubeletUrl": desired.KubeletURL,
			"kubectlUrl": desired.KubectlURL,
			"targetDir":  "/usr/local/bin",
		},
	}, nil
}

// Destroy removes kubelet and kubectl binaries.
func (Module) Destroy(_ context.Context, _ config.Config, req moduleapi.Request) error {
	if req.Logger != nil {
		req.Logger.Infof("client-tools destroy: removing kubelet and kubectl binaries")
	}
	_ = os.Remove("/usr/local/bin/kubelet")
	_ = os.Remove("/usr/local/bin/kubectl")
	_ = os.Remove("/srv/magnum/bin/kubectl")

	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:InstallClients", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"kubeletUrl": pulumi.String(kubeletURL(cfg)),
		"kubectlUrl": pulumi.String(kubectlURL(cfg)),
		"targetDir":  pulumi.String("/usr/local/bin"),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

func kubeletURL(cfg config.Config) string {
	return fmt.Sprintf("https://dl.k8s.io/release/%s/bin/linux/%s/kubelet", cfg.Shared.KubeTag, cfg.Shared.Arch)
}

func kubectlURL(cfg config.Config) string {
	return fmt.Sprintf("https://dl.k8s.io/release/%s/bin/linux/%s/kubectl", cfg.Shared.KubeTag, cfg.Shared.Arch)
}

func moduleStateDir(req moduleapi.Request) string {
	return filepath.Join(filepath.Dir(req.Paths.StateFile), "modules")
}

func moduleStateFile(req moduleapi.Request) string {
	return filepath.Join(moduleStateDir(req), "client-tools.json")
}

func loadState(path string) (installState, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return installState{}, nil
	}
	if err != nil {
		return installState{}, err
	}
	var state installState
	if err := json.Unmarshal(data, &state); err != nil {
		return installState{}, err
	}
	return state, nil
}

func saveState(path string, state installState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func binaryNeedsReconcile(path, desiredURL, installedURL, installedChecksum string) bool {
	if desiredURL == "" {
		return false
	}
	if desiredURL != installedURL || installedChecksum == "" {
		return true
	}
	currentChecksum, err := host.FileSHA256(path)
	if err != nil {
		return true
	}
	return currentChecksum != installedChecksum
}

func plannedBinaryChange(path, desiredURL string, needed bool, summary string) *host.Change {
	if !needed || desiredURL == "" {
		return nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return &host.Change{Action: host.ActionCreate, Path: path, Summary: summary}
	}
	return &host.Change{Action: host.ActionReplace, Path: path, Summary: summary}
}

func plannedCopyChange(src, dst string, mode os.FileMode) (*host.Change, error) {
	content, err := os.ReadFile(src)
	if err != nil {
		return nil, err
	}

	current, err := os.ReadFile(dst)
	switch {
	case os.IsNotExist(err):
		return &host.Change{Action: host.ActionCreate, Path: dst, Summary: fmt.Sprintf("copy %s to %s", src, dst)}, nil
	case err != nil:
		return nil, err
	}

	info, err := os.Stat(dst)
	if err != nil {
		return nil, err
	}
	if string(current) == string(content) && info.Mode().Perm() == mode.Perm() {
		return nil, nil
	}
	if string(current) == string(content) {
		return &host.Change{Action: host.ActionUpdate, Path: dst, Summary: fmt.Sprintf("update %s from %s", dst, src)}, nil
	}
	return &host.Change{Action: host.ActionReplace, Path: dst, Summary: fmt.Sprintf("replace %s from %s", dst, src)}, nil
}

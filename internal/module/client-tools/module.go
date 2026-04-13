package clienttools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

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

	KubeletURL pulumi.StringOutput `pulumi:"kubeletUrl"`
	KubectlURL pulumi.StringOutput `pulumi:"kubectlUrl"`
	HelmURL    pulumi.StringOutput `pulumi:"helmUrl"`
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
	HelmVersion       string `json:"helmVersion"`
	HelmURL           string `json:"helmUrl"`
	HelmSHA256        string `json:"helmSha256"`
	HelmCopySHA256    string `json:"helmCopySha256"`
}

const helmVersion = "v3.20.2"

func (Module) PhaseID() string {
	return "client-tools"
}
func (Module) Dependencies() []string { return []string{"ca-rotation"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	executor := host.NewExecutor(req.Apply, req.Logger)
	changes := make([]host.Change, 0)

	for _, dir := range []string{"/srv/magnum/bin", "/srv/magnum/k8s", moduleStateDir(req)} {
		result, err := (hostresource.DirectorySpec{Path: dir, Mode: 0o755}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, result.Changes...)
	}

	for _, line := range []string{
		"export PATH=/srv/magnum/bin:$PATH",
		"export HISTCONTROL=ignoredups",
	} {
		result, err := (hostresource.LineSpec{Path: "/root/.bashrc", Line: line, Mode: 0o644}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, result.Changes...)
	}

	desired := installState{
		KubeTag:     cfg.Shared.KubeTag,
		Arch:        cfg.Shared.Arch,
		KubeletURL:  kubeletURL(cfg),
		KubectlURL:  kubectlURL(cfg),
		HelmVersion: helmVersion,
		HelmURL:     helmURL(cfg),
	}

	installed, err := loadState(moduleStateFile(req))
	if err != nil {
		return moduleapi.Result{}, err
	}

	needsKubelet := binaryNeedsReconcile("/usr/local/bin/kubelet", desired.KubeletURL, installed.KubeletURL, installed.KubeletSHA256)
	needsKubectl := binaryNeedsReconcile("/usr/local/bin/kubectl", desired.KubectlURL, installed.KubectlURL, installed.KubectlSHA256)
	needsKubectlCopy := binaryNeedsReconcile("/srv/magnum/bin/kubectl", desired.KubectlURL, installed.KubectlURL, installed.KubectlCopySHA256) || needsKubectl
	needsHelm := binaryNeedsReconcile("/usr/local/bin/helm", desired.HelmURL, installed.HelmURL, installed.HelmSHA256)
	needsHelmCopy := binaryNeedsReconcile("/srv/magnum/bin/helm", desired.HelmURL, installed.HelmURL, installed.HelmCopySHA256) || needsHelm

	if req.Apply {
		kubeletDownload, kubectlDownload, err := downloadClientBinaries(ctx, executor, desired, needsKubelet, needsKubectl)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if needsKubelet {
			if kubeletDownload.Change != nil {
				changes = append(changes, *kubeletDownload.Change)
			}
			desired.KubeletSHA256 = kubeletDownload.Checksum
		} else {
			desired.KubeletSHA256, _ = host.FileSHA256("/usr/local/bin/kubelet")
		}
		if needsKubectl {
			if kubectlDownload.Change != nil {
				changes = append(changes, *kubectlDownload.Change)
			}
			desired.KubectlSHA256 = kubectlDownload.Checksum
		} else {
			desired.KubectlSHA256, _ = host.FileSHA256("/usr/local/bin/kubectl")
		}

		if needsKubectlCopy {
			copyResult, err := (hostresource.CopySpec{Source: "/usr/local/bin/kubectl", Path: "/srv/magnum/bin/kubectl", Mode: 0o755}).Apply(executor)
			if err != nil {
				return moduleapi.Result{}, err
			}
			changes = append(changes, copyResult.Changes...)
		}
		desired.KubectlCopySHA256, _ = host.FileSHA256("/srv/magnum/bin/kubectl")

		if needsHelm || needsHelmCopy {
			helmChanges, err := reconcileHelmBinary(executor, desired, needsHelm, needsHelmCopy)
			if err != nil {
				return moduleapi.Result{}, err
			}
			changes = append(changes, helmChanges...)
		}
		if needsHelm {
			desired.HelmSHA256, _ = host.FileSHA256("/usr/local/bin/helm")
		} else {
			desired.HelmSHA256, _ = host.FileSHA256("/usr/local/bin/helm")
		}
		if needsHelmCopy || needsHelm {
			desired.HelmCopySHA256, _ = host.FileSHA256("/srv/magnum/bin/helm")
		} else {
			desired.HelmCopySHA256, _ = host.FileSHA256("/srv/magnum/bin/helm")
		}

		if cfg.Shared.SELinuxMode == "enforcing" && (needsKubelet || needsKubectl || needsKubectlCopy || needsHelm || needsHelmCopy) {
			_ = executor.Run("chcon", "system_u:object_r:bin_t:s0", "/usr/local/bin/kubelet", "/usr/local/bin/kubectl", "/srv/magnum/bin/kubectl", "/usr/local/bin/helm", "/srv/magnum/bin/helm")
		}
		if err := saveState(moduleStateFile(req), desired); err != nil {
			return moduleapi.Result{}, err
		}
	} else {
		if needsKubelet {
			result, err := (hostresource.DownloadSpec{URL: desired.KubeletURL, Path: "/usr/local/bin/kubelet", Mode: 0o755, Retries: 5}).Apply(executor)
			if err != nil {
				return moduleapi.Result{}, err
			}
			changes = append(changes, result.Changes...)
		}
		if needsKubectl {
			result, err := (hostresource.DownloadSpec{URL: desired.KubectlURL, Path: "/usr/local/bin/kubectl", Mode: 0o755, Retries: 5}).Apply(executor)
			if err != nil {
				return moduleapi.Result{}, err
			}
			changes = append(changes, result.Changes...)
		}
		if needsKubectlCopy {
			copyResult, err := (hostresource.CopySpec{Source: "/usr/local/bin/kubectl", Path: "/srv/magnum/bin/kubectl", Mode: 0o755}).Apply(executor)
			if err != nil {
				return moduleapi.Result{}, err
			}
			changes = append(changes, copyResult.Changes...)
		}
		if needsHelm || needsHelmCopy {
			helmChanges, err := reconcileHelmBinary(executor, desired, needsHelm, needsHelmCopy)
			if err != nil {
				return moduleapi.Result{}, err
			}
			changes = append(changes, helmChanges...)
		}
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{
			"kubeletUrl": desired.KubeletURL,
			"kubectlUrl": desired.KubectlURL,
			"helmUrl":    desired.HelmURL,
			"targetDir":  "/usr/local/bin",
		},
	}, nil
}

func downloadClientBinaries(ctx context.Context, executor *host.Executor, desired installState, needsKubelet, needsKubectl bool) (host.DownloadResult, host.DownloadResult, error) {
	var wg sync.WaitGroup
	var kubeletDownload host.DownloadResult
	var kubectlDownload host.DownloadResult
	var kubeletErr error
	var kubectlErr error

	if needsKubelet {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kubeletDownload, kubeletErr = (hostresource.DownloadSpec{URL: desired.KubeletURL, Path: "/usr/local/bin/kubelet", Mode: 0o755, Retries: 5}).ApplyWithResultContext(ctx, executor)
		}()
	}
	if needsKubectl {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kubectlDownload, kubectlErr = (hostresource.DownloadSpec{URL: desired.KubectlURL, Path: "/usr/local/bin/kubectl", Mode: 0o755, Retries: 5}).ApplyWithResultContext(ctx, executor)
		}()
	}
	wg.Wait()

	if kubeletErr != nil {
		return host.DownloadResult{}, host.DownloadResult{}, kubeletErr
	}
	if kubectlErr != nil {
		return host.DownloadResult{}, host.DownloadResult{}, kubectlErr
	}
	return kubeletDownload, kubectlDownload, nil
}

func reconcileHelmBinary(executor *host.Executor, desired installState, needsHelm, needsHelmCopy bool) ([]host.Change, error) {
	var changes []host.Change

	for _, dir := range []string{helmExtractDirFromState(desired), filepath.Dir(helmArchivePathFromState(desired))} {
		result, err := (hostresource.DirectorySpec{Path: dir, Mode: 0o755}).Apply(executor)
		if err != nil {
			return nil, err
		}
		changes = append(changes, result.Changes...)
	}

	downloadResult, err := (hostresource.DownloadSpec{URL: desired.HelmURL, Path: helmArchivePathFromState(desired), Mode: 0o644, Retries: 5}).Apply(executor)
	if err != nil {
		return nil, err
	}
	changes = append(changes, downloadResult.Changes...)

	extractResult, err := (hostresource.ExtractTarSpec{
		ArchivePath: helmArchivePathFromState(desired),
		Destination: helmExtractDirFromState(desired),
		CheckPaths:  []string{helmExtractedBinaryPathFromState(desired)},
	}).Apply(executor)
	if err != nil {
		return nil, err
	}
	changes = append(changes, extractResult.Changes...)

	if needsHelmCopy {
		copyResult, err := (hostresource.CopySpec{Source: helmExtractedBinaryPathFromState(desired), Path: "/srv/magnum/bin/helm", Mode: 0o755}).Apply(executor)
		if err != nil {
			return nil, err
		}
		changes = append(changes, copyResult.Changes...)
	}
	if needsHelm {
		copyResult, err := (hostresource.CopySpec{Source: helmExtractedBinaryPathFromState(desired), Path: "/usr/local/bin/helm", Mode: 0o755}).Apply(executor)
		if err != nil {
			return nil, err
		}
		changes = append(changes, copyResult.Changes...)
	}

	return changes, nil
}

// Destroy removes kubelet and kubectl binaries.
func (Module) Destroy(_ context.Context, _ config.Config, req moduleapi.Request) error {
	if req.Logger != nil {
		req.Logger.Infof("client-tools destroy: removing kubelet, kubectl, and helm binaries")
	}
	_ = os.Remove("/usr/local/bin/kubelet")
	_ = os.Remove("/usr/local/bin/kubectl")
	_ = os.Remove("/srv/magnum/bin/kubectl")
	_ = os.Remove("/usr/local/bin/helm")
	_ = os.Remove("/srv/magnum/bin/helm")

	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:InstallClients", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := hostresource.ChildResourceOptions(res, opts...)
	var binDirRes pulumi.Resource
	for _, dir := range []string{"/srv/magnum/bin", "/srv/magnum/k8s"} {
		resDir, err := hostsdk.RegisterDirectorySpec(ctx, name+"-dir-"+filepath.Base(dir), hostresource.DirectorySpec{Path: dir, Mode: 0o755}, childOpts...)
		if err != nil {
			return nil, err
		}
		if dir == "/srv/magnum/bin" {
			binDirRes = resDir
		}
	}
	for _, line := range []struct {
		name string
		line string
	}{
		{name: "path-export", line: "export PATH=/srv/magnum/bin:$PATH"},
		{name: "histcontrol", line: "export HISTCONTROL=ignoredups"},
	} {
		if _, err := hostsdk.RegisterLineSpec(ctx, name+"-bashrc-"+line.name, hostresource.LineSpec{Path: "/root/.bashrc", Line: line.line, Mode: 0o644}, childOpts...); err != nil {
			return nil, err
		}
	}
	kubeletRes, err := hostsdk.RegisterDownloadSpec(ctx, name+"-kubelet", hostresource.DownloadSpec{URL: kubeletURL(cfg), Path: "/usr/local/bin/kubelet", Mode: 0o755, Retries: 5}, childOpts...)
	if err != nil {
		return nil, err
	}
	_ = kubeletRes
	kubectlRes, err := hostsdk.RegisterDownloadSpec(ctx, name+"-kubectl", hostresource.DownloadSpec{URL: kubectlURL(cfg), Path: "/usr/local/bin/kubectl", Mode: 0o755, Retries: 5}, childOpts...)
	if err != nil {
		return nil, err
	}
	copyOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, kubectlRes, binDirRes)
	if _, err := hostsdk.RegisterCopySpec(ctx, name+"-kubectl-copy", hostresource.CopySpec{Source: "/usr/local/bin/kubectl", Path: "/srv/magnum/bin/kubectl", Mode: 0o755}, copyOpts...); err != nil {
		return nil, err
	}
	helmExtractDirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-helm-dir", hostresource.DirectorySpec{Path: helmExtractDir(cfg), Mode: 0o755}, childOpts...)
	if err != nil {
		return nil, err
	}
	helmArchiveRes, err := hostsdk.RegisterDownloadSpec(ctx, name+"-helm-archive", hostresource.DownloadSpec{URL: helmURL(cfg), Path: helmArchivePath(cfg), Mode: 0o644, Retries: 5}, childOpts...)
	if err != nil {
		return nil, err
	}
	helmExtractOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, helmExtractDirRes, helmArchiveRes)
	helmExtractRes, err := hostsdk.RegisterExtractTarSpec(ctx, name+"-helm-extract", hostresource.ExtractTarSpec{ArchivePath: helmArchivePath(cfg), Destination: helmExtractDir(cfg), CheckPaths: []string{helmExtractedBinaryPath(cfg)}}, helmExtractOpts...)
	if err != nil {
		return nil, err
	}
	helmBinCopyOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, helmExtractRes, binDirRes)
	if _, err := hostsdk.RegisterCopySpec(ctx, name+"-helm-bin-copy", hostresource.CopySpec{Source: helmExtractedBinaryPath(cfg), Path: "/srv/magnum/bin/helm", Mode: 0o755}, helmBinCopyOpts...); err != nil {
		return nil, err
	}
	helmUsrCopyOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, helmExtractRes)
	if _, err := hostsdk.RegisterCopySpec(ctx, name+"-helm-usr-copy", hostresource.CopySpec{Source: helmExtractedBinaryPath(cfg), Path: "/usr/local/bin/helm", Mode: 0o755}, helmUsrCopyOpts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"kubeletUrl": pulumi.String(kubeletURL(cfg)),
		"kubectlUrl": pulumi.String(kubectlURL(cfg)),
		"helmUrl":    pulumi.String(helmURL(cfg)),
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

func helmURL(cfg config.Config) string {
	arch := normalizedHelmArch(cfg.Shared.Arch)
	return fmt.Sprintf("https://get.helm.sh/helm-%s-linux-%s.tar.gz", helmVersion, arch)
}

func helmArchivePath(cfg config.Config) string {
	arch := normalizedHelmArch(cfg.Shared.Arch)
	return filepath.Join("/srv/magnum/k8s", fmt.Sprintf("helm-%s-linux-%s.tar.gz", helmVersion, arch))
}

func helmArchivePathFromState(state installState) string {
	return filepath.Join("/srv/magnum/k8s", fmt.Sprintf("helm-%s-linux-%s.tar.gz", state.HelmVersion, normalizedHelmArch(state.Arch)))
}

func helmExtractDir(cfg config.Config) string {
	arch := normalizedHelmArch(cfg.Shared.Arch)
	return filepath.Join("/srv/magnum/k8s", fmt.Sprintf("helm-%s-linux-%s", helmVersion, arch))
}

func helmExtractDirFromState(state installState) string {
	return filepath.Join("/srv/magnum/k8s", fmt.Sprintf("helm-%s-linux-%s", state.HelmVersion, normalizedHelmArch(state.Arch)))
}

func helmExtractedBinaryPath(cfg config.Config) string {
	arch := normalizedHelmArch(cfg.Shared.Arch)
	return filepath.Join(helmExtractDir(cfg), fmt.Sprintf("linux-%s", arch), "helm")
}

func helmExtractedBinaryPathFromState(state installState) string {
	arch := normalizedHelmArch(state.Arch)
	return filepath.Join(helmExtractDirFromState(state), fmt.Sprintf("linux-%s", arch), "helm")
}

func normalizedHelmArch(arch string) string {
	switch arch {
	case "", "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return arch
	}
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

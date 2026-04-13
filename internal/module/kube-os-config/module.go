package kubeosconfig

import (
	"context"
	"fmt"
	"os"
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

	CloudConfigPath pulumi.StringOutput `pulumi:"cloudConfigPath"`
	OCCMConfigPath  pulumi.StringOutput `pulumi:"occmConfigPath"`
	CloudConfig     pulumi.StringOutput `pulumi:"cloudConfig"`
	OCCMConfig      pulumi.StringOutput `pulumi:"occmConfig"`
}

func (Module) PhaseID() string {
	return "kube-os-config"
}
func (Module) Dependencies() []string { return []string{"ca-rotation"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	executor := host.NewExecutor(req.Apply, req.Logger)
	changes := make([]host.Change, 0)

	dirResource := hostresource.DirectorySpec{Path: "/etc/kubernetes", Mode: 0o755}
	dirResult, err := dirResource.Apply(executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, dirResult.Changes...)

	caBundle := hostresource.CopySpec{
		Source: "/etc/pki/tls/certs/ca-bundle.crt",
		Path:   "/etc/kubernetes/ca-bundle.crt",
		Mode:   0o644,
	}
	copyResult, err := caBundle.Apply(executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, copyResult.Changes...)

	cloudConfig := buildCloudConfig(cfg)
	occmConfig := buildOCCMConfig(cfg)

	for _, file := range []struct {
		path    string
		content string
	}{
		{path: "/etc/kubernetes/cloud-config", content: cloudConfig},
		{path: "/etc/kubernetes/kube_openstack_config", content: cloudConfig},
		{path: "/etc/kubernetes/cloud-config-occm", content: occmConfig},
	} {
		fileSpec := hostresource.FileSpec{Path: file.path, Mode: 0o644, Absent: file.content == ""}
		if file.content == "" {
		} else {
			// These files are read from hostPath mounts by in-cluster pods such as
			// cluster-autoscaler, so root-only permissions break them with EACCES.
			fileSpec.Content = []byte(file.content)
		}
		fileResult, err := fileSpec.Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, fileResult.Changes...)
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{
			"cloudConfigPath": "/etc/kubernetes/cloud-config",
			"occmConfigPath":  "/etc/kubernetes/cloud-config-occm",
			"cloudConfig":     cloudConfig,
			"occmConfig":      occmConfig,
		},
	}, nil
}

// Destroy removes OpenStack cloud config files.
func (Module) Destroy(_ context.Context, _ config.Config, req moduleapi.Request) error {
	if req.Logger != nil {
		req.Logger.Infof("kube-os-config destroy: removing cloud config files")
	}
	_ = os.Remove("/etc/kubernetes/cloud-config")
	_ = os.Remove("/etc/kubernetes/kube_openstack_config")
	_ = os.Remove("/etc/kubernetes/cloud-config-occm")
	_ = os.Remove("/etc/kubernetes/ca-bundle.crt")

	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:WriteKubeOSConfig", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	cloudConfig := buildCloudConfig(cfg)
	occmConfig := buildOCCMConfig(cfg)

	dirResource := hostresource.DirectorySpec{Path: "/etc/kubernetes", Mode: 0o755}
	dirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-dir", dirResource, childOpts...)
	if err != nil {
		return nil, err
	}
	caBundle := hostresource.CopySpec{
		Source: "/etc/pki/tls/certs/ca-bundle.crt",
		Path:   "/etc/kubernetes/ca-bundle.crt",
		Mode:   0o644,
	}
	copyOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, dirRes)
	if _, err := hostsdk.RegisterCopySpec(ctx, name+"-ca-bundle", caBundle, copyOpts...); err != nil {
		return nil, err
	}
	for _, file := range []struct {
		name    string
		path    string
		content string
	}{
		{name: "cloud-config", path: "/etc/kubernetes/cloud-config", content: cloudConfig},
		{name: "kube-openstack-config", path: "/etc/kubernetes/kube_openstack_config", content: cloudConfig},
		{name: "cloud-config-occm", path: "/etc/kubernetes/cloud-config-occm", content: occmConfig},
	} {
		fileSpec := hostresource.FileSpec{Path: file.path, Mode: 0o644, Absent: file.content == ""}
		if file.content != "" {
			fileSpec.Content = []byte(file.content)
		}
		fileOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, dirRes)
		if _, err := hostsdk.RegisterFileSpec(ctx, name+"-"+file.name, fileSpec, fileOpts...); err != nil {
			return nil, err
		}
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"cloudConfigPath": pulumi.String("/etc/kubernetes/cloud-config"),
		"occmConfigPath":  pulumi.String("/etc/kubernetes/cloud-config-occm"),
		"cloudConfig":     pulumi.String(cloudConfig),
		"occmConfig":      pulumi.String(occmConfig),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

func buildCloudConfig(cfg config.Config) string {
	if cfg.Shared.TrustID == "" {
		return ""
	}

	lines := []string{
		"[Global]",
		fmt.Sprintf("auth-url=%s", cfg.Shared.AuthURL),
		fmt.Sprintf("user-id=%s", cfg.Shared.TrusteeUserID),
		fmt.Sprintf("password=%s", cfg.Shared.TrusteePassword),
		fmt.Sprintf("trust-id=%s", cfg.Shared.TrustID),
		"ca-file=/etc/kubernetes/ca-bundle.crt",
	}
	if region := cfg.Raw["REGION_NAME"]; region != "" {
		lines = append(lines, fmt.Sprintf("region=%s", region))
	}
	lines = append(lines,
		"[LoadBalancer]",
		fmt.Sprintf("use-octavia=%t", cfg.Shared.OctaviaEnabled),
		fmt.Sprintf("subnet-id=%s", cfg.Shared.ClusterSubnet),
		fmt.Sprintf("floating-network-id=%s", cfg.Shared.ExternalNetworkID),
		"create-monitor=yes",
		"monitor-delay=1m",
		"monitor-timeout=30s",
		"monitor-max-retries=3",
		"[BlockStorage]",
		"bs-version=v2",
	)
	return strings.Join(lines, "\n") + "\n"
}

func buildOCCMConfig(cfg config.Config) string {
	cloudConfig := buildCloudConfig(cfg)
	if cloudConfig == "" {
		return ""
	}
	return cloudConfig + "[Networking]\ninternal-network-name=" + cfg.Shared.ClusterNetworkName + "\n"
}

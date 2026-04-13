package proxy

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

	ServiceDirectory pulumi.StringOutput    `pulumi:"serviceDirectory"`
	RuntimeService   pulumi.StringOutput    `pulumi:"runtimeService"`
	DropInFiles      pulumi.StringMapOutput `pulumi:"dropInFiles"`
	BashExports      pulumi.StringMapOutput `pulumi:"bashExports"`
}

func (Module) PhaseID() string {
	return "proxy-env"
}
func (Module) Dependencies() []string { return []string{"storage"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	executor := host.NewExecutor(req.Apply, req.Logger)
	changes := make([]host.Change, 0)

	serviceDirectory := "/etc/systemd/system/docker.service.d"
	runtimeService := "docker.service"
	if cfg.Shared.ContainerRuntime == "containerd" {
		serviceDirectory = "/etc/systemd/system/containerd.service.d"
		runtimeService = "containerd.service"
	}

	dropInDir := hostresource.DirectorySpec{Path: serviceDirectory, Mode: 0o755}
	dirResult, err := dropInDir.Apply(executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, dirResult.Changes...)

	runtimeChanged := false
	for _, item := range []struct {
		path    string
		envName string
		value   string
		bashVar string
	}{
		{path: serviceDirectory + "/http_proxy.conf", envName: "HTTP_PROXY", value: cfg.Shared.HTTPProxy, bashVar: "http_proxy"},
		{path: serviceDirectory + "/https_proxy.conf", envName: "HTTPS_PROXY", value: cfg.Shared.HTTPSProxy, bashVar: "https_proxy"},
		{path: serviceDirectory + "/no_proxy.conf", envName: "NO_PROXY", value: cfg.Shared.NoProxy, bashVar: "no_proxy"},
	} {
		fileSpec := hostresource.FileSpec{Path: item.path, Mode: 0o644, Absent: item.value == ""}
		if item.value != "" {
			fileSpec.Content = []byte(fmt.Sprintf("[Service]\nEnvironment=%s=%s\n", item.envName, item.value))
		}
		fileResult, err := fileSpec.Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, fileResult.Changes...)
		runtimeChanged = runtimeChanged || fileResult.Changed

		exportSpec := hostresource.ExportSpec{Path: "/root/.bashrc", VarName: item.bashVar, Value: item.value, Mode: 0o644}
		exportResult, err := exportSpec.Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, exportResult.Changes...)
	}

	if runtimeChanged {
		serviceResource := hostresource.SystemdServiceSpec{
			Unit:            runtimeService,
			DaemonReload:    true,
			Restart:         true,
			RestartOnChange: true,
			RestartReason:   "proxy configuration changed",
			RestartToken:    hostresource.BytesSHA256([]byte(strings.Join([]string{cfg.Shared.HTTPProxy, cfg.Shared.HTTPSProxy, cfg.Shared.NoProxy}, "\x00"))),
		}
		serviceResult, err := serviceResource.Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, serviceResult.Changes...)
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{
			"serviceDirectory": serviceDirectory,
			"runtimeService":   runtimeService,
		},
	}, nil
}

// Destroy removes proxy drop-in configuration files.
func (Module) Destroy(_ context.Context, _ config.Config, req moduleapi.Request) error {
	if req.Logger != nil {
		req.Logger.Infof("proxy-env destroy: removing docker service proxy drop-ins")
	}
	_ = os.Remove("/etc/systemd/system/docker.service.d/http_proxy.conf")
	_ = os.Remove("/etc/systemd/system/docker.service.d/https_proxy.conf")
	_ = os.Remove("/etc/systemd/system/docker.service.d/no_proxy.conf")

	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:Proxy", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := hostresource.ChildResourceOptions(res, opts...)

	serviceDirectory := "/etc/systemd/system/docker.service.d"
	runtimeService := "docker.service"
	if cfg.Shared.ContainerRuntime == "containerd" {
		serviceDirectory = "/etc/systemd/system/containerd.service.d"
		runtimeService = "containerd.service"
	}

	dropInDir := hostresource.DirectorySpec{Path: serviceDirectory, Mode: 0o755}
	dropInDirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-dropin-dir", dropInDir, childOpts...)
	if err != nil {
		return nil, err
	}

	dropIns := pulumi.StringMap{}
	bashExports := pulumi.StringMap{}
	dropInResources := []pulumi.Resource{dropInDirRes}
	for _, item := range []struct {
		name    string
		envName string
		value   string
		bashVar string
	}{
		{name: "http", envName: "HTTP_PROXY", value: cfg.Shared.HTTPProxy, bashVar: "http_proxy"},
		{name: "https", envName: "HTTPS_PROXY", value: cfg.Shared.HTTPSProxy, bashVar: "https_proxy"},
		{name: "no", envName: "NO_PROXY", value: cfg.Shared.NoProxy, bashVar: "no_proxy"},
	} {
		fileSpec := hostresource.FileSpec{Path: serviceDirectory + "/" + item.name + "_proxy.conf", Mode: 0o644, Absent: item.value == ""}
		if item.value != "" {
			fileSpec.Content = []byte(fmt.Sprintf("[Service]\nEnvironment=%s=%s\n", item.envName, item.value))
		}
		fileOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, dropInDirRes)
		fileRes, err := hostsdk.RegisterFileSpec(ctx, name+"-"+item.name+"-dropin", fileSpec, fileOpts...)
		if err != nil {
			return nil, err
		}
		dropInResources = append(dropInResources, fileRes)
		exportSpec := hostresource.ExportSpec{Path: "/root/.bashrc", VarName: item.bashVar, Value: item.value, Mode: 0o644}
		if _, err := hostsdk.RegisterExportSpec(ctx, name+"-"+item.name+"-export", exportSpec, childOpts...); err != nil {
			return nil, err
		}
	}

	serviceResource := hostresource.SystemdServiceSpec{
		Unit:            runtimeService,
		DaemonReload:    true,
		RestartOnChange: true,
		RestartReason:   "proxy configuration changed",
		RestartToken:    hostresource.BytesSHA256([]byte(strings.Join([]string{cfg.Shared.HTTPProxy, cfg.Shared.HTTPSProxy, cfg.Shared.NoProxy}, "\x00"))),
	}
	serviceOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, dropInResources...)
	if _, err := hostsdk.RegisterSystemdServiceSpec(ctx, name+"-runtime-service", serviceResource, serviceOpts...); err != nil {
		return nil, err
	}

	if cfg.Shared.HTTPProxy != "" {
		dropIns["http_proxy.conf"] = pulumi.String(fmt.Sprintf("[Service]\nEnvironment=HTTP_PROXY=%s\n", cfg.Shared.HTTPProxy))
		bashExports["http_proxy"] = pulumi.String(cfg.Shared.HTTPProxy)
	}
	if cfg.Shared.HTTPSProxy != "" {
		dropIns["https_proxy.conf"] = pulumi.String(fmt.Sprintf("[Service]\nEnvironment=HTTPS_PROXY=%s\n", cfg.Shared.HTTPSProxy))
		bashExports["https_proxy"] = pulumi.String(cfg.Shared.HTTPSProxy)
	}
	if cfg.Shared.NoProxy != "" {
		dropIns["no_proxy.conf"] = pulumi.String(fmt.Sprintf("[Service]\nEnvironment=NO_PROXY=%s\n", cfg.Shared.NoProxy))
		bashExports["no_proxy"] = pulumi.String(cfg.Shared.NoProxy)
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"serviceDirectory": pulumi.String(serviceDirectory),
		"runtimeService":   pulumi.String(runtimeService),
		"dropInFiles":      dropIns,
		"bashExports":      bashExports,
	}); err != nil {
		return nil, err
	}
	return res, nil
}

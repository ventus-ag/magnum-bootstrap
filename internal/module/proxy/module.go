package proxy

import (
	"context"
	"fmt"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
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

	change, err := executor.EnsureDir(serviceDirectory, 0o755)
	if err != nil {
		return moduleapi.Result{}, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

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
		if item.value != "" {
			content := fmt.Sprintf("[Service]\nEnvironment=%s=%s\n", item.envName, item.value)
			change, err := executor.EnsureFile(item.path, []byte(content), 0o644)
			if err != nil {
				return moduleapi.Result{}, err
			}
			if change != nil {
				changes = append(changes, *change)
				runtimeChanged = true
			}
		} else {
			change, err := executor.EnsureAbsent(item.path)
			if err != nil {
				return moduleapi.Result{}, err
			}
			if change != nil {
				changes = append(changes, *change)
				runtimeChanged = true
			}
		}

		change, err := executor.UpsertExport("/root/.bashrc", item.bashVar, item.value, 0o644)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	if runtimeChanged {
		changes = append(changes,
			host.Change{Action: host.ActionReload, Path: "systemd", Summary: "reload systemd manager configuration"},
			host.Change{Action: host.ActionRestart, Path: runtimeService, Summary: fmt.Sprintf("restart %s", runtimeService)},
		)
		if req.Apply {
			if err := executor.Run("systemctl", "daemon-reload"); err != nil {
				return moduleapi.Result{}, err
			}
			if err := executor.Run("systemctl", "restart", runtimeService); err != nil {
				return moduleapi.Result{}, err
			}
		}
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

	serviceDirectory := "/etc/systemd/system/docker.service.d"
	runtimeService := "docker.service"
	if cfg.Shared.ContainerRuntime == "containerd" {
		serviceDirectory = "/etc/systemd/system/containerd.service.d"
		runtimeService = "containerd.service"
	}

	dropIns := pulumi.StringMap{}
	bashExports := pulumi.StringMap{}
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

package prereqvalidation

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/kubeletconfig"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState

	Role      pulumi.StringOutput `pulumi:"role"`
	Operation pulumi.StringOutput `pulumi:"operation"`
	Instance  pulumi.StringOutput `pulumi:"instance"`
	Validated pulumi.StringOutput `pulumi:"validated"`
}

func (Module) PhaseID() string {
	return "prereq-validation"
}
func (Module) Dependencies() []string { return nil }

func (Module) Run(_ context.Context, cfg config.Config, _ moduleapi.Request) (moduleapi.Result, error) {
	validated := []string{"INSTANCE_NAME", "NODEGROUP_ROLE", "KUBE_TAG", "ARCH"}
	var warnings []string

	switch cfg.Role() {
	case config.RoleMaster:
		if cfg.Master == nil {
			return moduleapi.Result{}, fmt.Errorf("master role selected without master configuration")
		}
		if cfg.Master.KubeAPIPublicAddress == "" && cfg.Master.KubeAPIPrivateAddress == "" {
			return moduleapi.Result{}, fmt.Errorf("master configuration is missing API addresses")
		}
	case config.RoleWorker:
		if cfg.Worker == nil {
			return moduleapi.Result{}, fmt.Errorf("worker role selected without worker configuration")
		}
		if cfg.Worker.KubeMasterIP == "" {
			return moduleapi.Result{}, fmt.Errorf("worker configuration is missing KUBE_MASTER_IP")
		}
	default:
		return moduleapi.Result{}, fmt.Errorf("unable to determine node role from heat-params")
	}

	if cfg.Shared.InstanceName == "" {
		return moduleapi.Result{}, fmt.Errorf("INSTANCE_NAME is required")
	}
	if cfg.Shared.KubeTag == "" {
		return moduleapi.Result{}, fmt.Errorf("KUBE_TAG is required")
	}
	if cfg.Shared.Arch == "" {
		return moduleapi.Result{}, fmt.Errorf("ARCH is required")
	}

	// cgroup v1 detection: K8s 1.35+ refuses to start on cgroup v1 nodes.
	if isCgroupV1() {
		if kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 35) {
			return moduleapi.Result{}, fmt.Errorf(
				"cgroup v1 detected but Kubernetes >= 1.35 requires cgroup v2; "+
					"kubelet will refuse to start on this node")
		}
		if kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 33) {
			// K8s 1.33-1.34 emit warnings; log but don't block.
			warnings = append(warnings, "cgroup v1 detected; Kubernetes >= 1.33 "+
				"deprecates cgroup v1 and >= 1.35 will refuse to start")
		}
	}

	// Docker runtime is not CRI-compliant for K8s >= 1.34.
	if cfg.Shared.ContainerRuntime == "docker" && kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 34) {
		return moduleapi.Result{}, fmt.Errorf(
			"container runtime \"docker\" is not supported for Kubernetes >= 1.34; "+
				"use \"containerd\" or another CRI-compliant runtime")
	}

	outputs := map[string]string{
		"validated": strings.Join(validated, ","),
	}
	if len(warnings) > 0 {
		outputs["warnings"] = strings.Join(warnings, "; ")
	}

	return moduleapi.Result{
		Outputs: outputs,
	}, nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:PrereqValidation", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"role":      pulumi.String(cfg.Role().String()),
		"operation": pulumi.String(cfg.Operation().String()),
		"instance":  pulumi.String(cfg.Shared.InstanceName),
		"validated": pulumi.String("INSTANCE_NAME,NODEGROUP_ROLE,KUBE_TAG,ARCH"),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

// isCgroupV1 returns true if the node is running cgroup v1.
// /sys/fs/cgroup/cgroup.controllers exists only on cgroup v2.
func isCgroupV1() bool {
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	return os.IsNotExist(err)
}

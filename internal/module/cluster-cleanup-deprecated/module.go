package clustercleanupdeprecated

import (
	"context"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "cluster-cleanup-deprecated" }
func (Module) Dependencies() []string { return []string{"cluster-rbac"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if !cfg.IsFirstMaster() {
		return moduleapi.Result{}, nil
	}
	if req.Apply {
		executor := host.NewExecutor(req.Apply, req.Logger)
		cleanupDeprecatedFlannel(executor)
	}
	return moduleapi.Result{
		Outputs: map[string]string{"firstMaster": "true"},
	}, nil
}

// cleanupDeprecatedFlannel removes only the legacy flannel resources that the
// old Magnum cleanup script deleted before upgrade.
func cleanupDeprecatedFlannel(executor *host.Executor) {
	if executor == nil {
		return
	}

	legacyDeletes := [][]string{
		{"kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", "delete", "daemonset", "kube-flannel-ds", "-n", "kube-system", "--ignore-not-found=true"},
		{"kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", "delete", "configmap", "kube-flannel-cfg", "-n", "kube-system", "--ignore-not-found=true"},
		{"kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", "delete", "serviceaccount", "flannel", "-n", "kube-system", "--ignore-not-found=true"},
		{"kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", "delete", "clusterrolebinding", "flannel", "--ignore-not-found=true"},
		{"kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", "delete", "clusterrole", "flannel", "--ignore-not-found=true"},
	}
	for _, cmd := range legacyDeletes {
		_ = executor.Run(cmd[0], cmd[1:]...)
	}
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:CleanupDeprecated", name, res, opts...); err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"firstMaster":          pulumi.Bool(heat.Cfg.IsFirstMaster()),
		"cleanupDeprecated":    pulumi.Bool(true),
		"bestEffortImperative": pulumi.Bool(true),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

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

func (Module) PhaseID() string { return "cluster-cleanup-deprecated" }

// Depends on cluster-flannel (which installs the replacement Helm flannel), so
// this module — which deletes the LEGACY kube-system flannel — is ordered AFTER
// the replacement is registered. The old edge (cluster-flannel depending on
// this module) deleted the live CNI before its replacement existed, a
// cluster-wide network outage on the first reconcile of any pre-Helm cluster.
func (Module) Dependencies() []string { return []string{"cluster-flannel"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if !cfg.IsFirstMaster() {
		return moduleapi.Result{}, nil
	}
	if req.Apply {
		executor := host.NewExecutor(req.Apply, req.Logger)
		cleanupDeprecatedFlannel(executor, cfg)
	}
	return moduleapi.Result{
		Outputs: map[string]string{"firstMaster": "true"},
	}, nil
}

// cleanupDeprecatedFlannel removes the legacy pre-Helm flannel (a manifest
// DaemonSet in kube-system) that older Magnum clusters shipped, now replaced by
// the Helm flannel release in the kube-flannel namespace.
//
// SAFETY — the legacy kube-system flannel IS the live CNI on a not-yet-migrated
// cluster. Deleting it before the replacement is serving fails every pod
// sandbox on every node (cluster-wide outage). This module is ordered after
// cluster-flannel and waits, bounded, for the replacement DaemonSet to roll
// out before deleting the old one. If the replacement is NOT ready within the
// timeout, the legacy flannel is KEPT and the delete is retried on the next
// reconcile — a brief double-flannel converges safely; a CNI gap does not.
//
// This still removes the old flannel and runs on create, upgrade and CA
// rotation (every first-master reconcile): once the replacement is steady the
// kube-system deletes are simple no-ops (--ignore-not-found).
func cleanupDeprecatedFlannel(executor *host.Executor, cfg config.Config) {
	if executor == nil {
		return
	}

	// Only flannel clusters have a legacy flannel to migrate. For any other CNI
	// (e.g. calico) there is nothing to delete and no replacement to wait for —
	// and we must never touch kube-system here.
	if cfg.Shared.NetworkDriver != "flannel" {
		return
	}

	const kubeconfig = "--kubeconfig=/etc/kubernetes/admin.conf"

	// Wait for the replacement Helm flannel to be serving before removing the
	// legacy one. rollout status polls live cluster state, so it observes the
	// in-flight Helm install (registered by cluster-flannel just before this
	// phase) converging. On a steady cluster this returns immediately.
	if err := executor.Run("kubectl", kubeconfig, "-n", "kube-flannel",
		"rollout", "status", "daemonset/kube-flannel-ds", "--timeout=180s"); err != nil {
		if executor.Logger != nil {
			executor.Logger.Warnf("cluster-cleanup-deprecated: replacement flannel kube-flannel/kube-flannel-ds not ready (%v); keeping legacy kube-system flannel to avoid a CNI gap, will retry next reconcile", err)
		}
		return
	}

	legacyDeletes := [][]string{
		{kubeconfig, "delete", "daemonset", "kube-flannel-ds", "-n", "kube-system", "--ignore-not-found=true"},
		{kubeconfig, "delete", "configmap", "kube-flannel-cfg", "-n", "kube-system", "--ignore-not-found=true"},
		{kubeconfig, "delete", "serviceaccount", "flannel", "-n", "kube-system", "--ignore-not-found=true"},
	}
	for _, args := range legacyDeletes {
		_ = executor.Run("kubectl", args...)
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

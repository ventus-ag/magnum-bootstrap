package clusterautohealer

import (
	"context"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "cluster-auto-healer" }
func (Module) Dependencies() []string { return []string{"cluster-rbac"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	return clusterhelm.RunNoop(ctx, cfg, req, cfg.Shared.AutoHealingEnabled, "npd", "kube-system")
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	if !cfg.IsFirstMaster() || !cfg.Shared.AutoHealingEnabled {
		return clusterhelm.RegisterSkipped(ctx, "magnum:cluster:AutoHealer", name, opts...)
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:AutoHealer", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/"
	}

	// Node Problem Detector via Helm.
	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-npd", clusterhelm.HelmReleaseArgs{
		ReleaseName: "npd",
		Namespace:   "kube-system",
		Chart:       "node-problem-detector",
		Version:     "2.3.4",
		RepoURL:     "https://charts.deliveryhero.io/",
		Values: map[string]interface{}{
			"fullnameOverride": "node-problem-detector",
			"image": map[string]interface{}{
				"repository": prefix + "node-problem-detector/node-problem-detector",
			},
			"priorityClassName": "system-node-critical",
			"settings": map[string]interface{}{
				"log_monitors": []interface{}{
					"/config/kernel-monitor.json",
					"/config/docker-monitor.json",
				},
				"prometheus_address": "0.0.0.0",
				"prometheus_port":    20257,
				"heartBeatPeriod":    "5m0s",
			},
			"logDir": map[string]interface{}{
				"host": "/var/log/",
				"pod":  "",
			},
			"securityContext": map[string]interface{}{
				"privileged": true,
			},
			"tolerations": []interface{}{
				map[string]interface{}{
					"effect":   "NoSchedule",
					"operator": "Exists",
				},
			},
			"metrics": map[string]interface{}{
				"enabled": false,
				"serviceMonitor": map[string]interface{}{
					"enabled": false,
				},
				"prometheusRule": map[string]interface{}{
					"enabled": false,
				},
			},
			"updateStrategy":  "RollingUpdate",
			"maxUnavailable":  1,
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"autoHealingEnabled": pulumi.Bool(true),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

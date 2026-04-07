package clusterautoscaler

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

func (Module) PhaseID() string { return "cluster-autoscaler" }
func (Module) Dependencies() []string { return []string{"cluster-rbac"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	return clusterhelm.RunNoop(ctx, cfg, req, cfg.Shared.AutoScalingEnabled, "openstack-autoscaler", "kube-system")
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	if !cfg.IsFirstMaster() || !cfg.Shared.AutoScalingEnabled {
		return clusterhelm.RegisterSkipped(ctx, "magnum:cluster:Autoscaler", name, opts...)
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:Autoscaler", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	roleName := cfg.Shared.LeadNodeRoleName
	if roleName == "" {
		roleName = "master"
	}

	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/autoscaling/"
	}

	chartVersion := cfg.Shared.AutoscalerChartTag
	if chartVersion == "" {
		chartVersion = "9.29.1"
	}

	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-chart", clusterhelm.HelmReleaseArgs{
		ReleaseName: "openstack-autoscaler",
		Namespace:   "kube-system",
		Chart:       "cluster-autoscaler",
		Version:     chartVersion,
		RepoURL:     "https://kubernetes.github.io/autoscaler",
		Values: map[string]interface{}{
			"magnumClusterName": cfg.Shared.ClusterUUID,
			"image": map[string]interface{}{
				"repository": prefix + "cluster-autoscaler",
			},
			"cloudProvider":   "magnum",
			"nameOverride":    "manager",
			"cloudConfigPath": "/etc/kubernetes/cloud-config",
			"autoDiscovery": map[string]interface{}{
				"clusterName": cfg.Shared.ClusterUUID,
				"roles":       []interface{}{"worker"},
			},
			"extraArgs": map[string]interface{}{
				"logtostderr":                  true,
				"stderrthreshold":              "info",
				"v":                            4,
				"leader-elect-lease-duration":  "40s",
				"leader-elect-renew-deadline":  "20s",
			},
			"nodeSelector": map[string]interface{}{
				"node-role.kubernetes.io/" + roleName: "",
			},
			"tolerations": []interface{}{
				map[string]interface{}{
					"effect":   "NoSchedule",
					"operator": "Exists",
				},
				map[string]interface{}{
					"key":      "CriticalAddonsOnly",
					"operator": "Exists",
				},
				map[string]interface{}{
					"effect":   "NoExecute",
					"operator": "Exists",
				},
			},
			"dnsPolicy":         "Default",
			"priorityClassName": "system-cluster-critical",
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"autoScalingEnabled": pulumi.Bool(true),
		"clusterUuid":        pulumi.String(cfg.Shared.ClusterUUID),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

package clusterautoscaler

import (
	"context"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	clusterhelm "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type autoscalerVersion struct {
	imageTag     string // container image tag, e.g. "1.31.5"
	chartVersion string // Helm chart version, e.g. "9.38.0"
}

// autoscalerVersions maps Kubernetes minor version to the matching
// cluster-autoscaler image tag and Helm chart version.
// Source: https://github.com/kubernetes/autoscaler/releases
var autoscalerVersions = map[string]autoscalerVersion{
	"1.35": {"1.35.0", "9.55.1"},
	"1.34": {"1.34.3", "9.51.0"},
	"1.33": {"1.33.4", "9.47.0"},
	"1.32": {"1.32.7", "9.45.1"},
	"1.31": {"1.31.5", "9.38.0"},
	"1.30": {"1.30.7", "9.37.0"},
	"1.29": {"1.29.5", "9.35.0"},
	"1.28": {"1.28.7", "9.34.1"},
	"1.27": {"1.27.8", "9.29.5"},
	"1.26": {"1.26.8", "9.28.0"},
	"1.25": {"1.25.3", "9.25.0"},
	"1.24": {"1.24.3", "9.25.0"},
	"1.23": {"1.23.1", "9.14.0"},
	"1.22": {"1.22.3", "9.10.9"},
	"1.21": {"1.21.3", "9.10.9"},
	"1.20": {"1.20.3", "9.5.0"},
	"1.19": {"1.19.3", "9.0.0"},
	"1.18": {"1.18.3", "9.0.0"},
}

// autoscalerVersionForKube returns the cluster-autoscaler image tag and Helm
// chart version that match the given Kubernetes version string (e.g.
// "v1.31.5" or "1.31.5"). Falls back to sensible defaults if the version is
// unrecognised.
func autoscalerVersionForKube(kubeVersion string) autoscalerVersion {
	v := strings.TrimPrefix(kubeVersion, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) >= 2 {
		minor := parts[0] + "." + parts[1]
		if ver, ok := autoscalerVersions[minor]; ok {
			return ver
		}
	}
	return autoscalerVersion{imageTag: "1.27.8", chartVersion: "9.29.5"}
}

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "cluster-autoscaler" }
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

	ver := autoscalerVersionForKube(cfg.Shared.KubeVersion)

	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-chart", clusterhelm.HelmReleaseArgs{
		ReleaseName: "openstack-autoscaler",
		Namespace:   "kube-system",
		Chart:       "cluster-autoscaler",
		Version:     ver.chartVersion,
		RepoURL:     "https://kubernetes.github.io/autoscaler",
		Values: map[string]interface{}{
			"magnumClusterName": cfg.Shared.ClusterUUID,
			"image": map[string]interface{}{
				"repository": prefix + "cluster-autoscaler",
				"tag":        "v" + ver.imageTag,
			},
			"cloudProvider":   "magnum",
			"nameOverride":    "manager",
			"cloudConfigPath": "/etc/kubernetes/cloud-config",
			"autoDiscovery": map[string]interface{}{
				"clusterName": cfg.Shared.ClusterUUID,
				"roles":       []interface{}{"worker"},
			},
			"extraArgs": map[string]interface{}{
				"logtostderr":                 true,
				"stderrthreshold":             "info",
				"v":                           4,
				"leader-elect-lease-duration": "40s",
				"leader-elect-renew-deadline": "20s",
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

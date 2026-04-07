package clusteroccm

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

func (Module) PhaseID() string { return "cluster-occm" }
func (Module) Dependencies() []string { return []string{"cluster-rbac"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	return clusterhelm.RunNoop(ctx, cfg, req, cfg.Shared.CloudProviderEnabled, "openstack-ccm", "kube-system")
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	if !cfg.IsFirstMaster() || !cfg.Shared.CloudProviderEnabled {
		return clusterhelm.RegisterSkipped(ctx, "magnum:cluster:OCCM", name, opts...)
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:OCCM", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	roleName := cfg.Shared.LeadNodeRoleName
	if roleName == "" {
		roleName = "master"
	}

	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/provider-os/openstack-cloud-controller-manager"
	}

	chartVersion := cfg.Shared.OCCMChartTag
	if chartVersion == "" {
		chartVersion = "2.27.1"
	}

	imageTag := cfg.Shared.OCCMImageTag
	if imageTag == "" {
		imageTag = "v1.24.6"
	}

	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-chart", clusterhelm.HelmReleaseArgs{
		ReleaseName: "openstack-ccm",
		Namespace:   "kube-system",
		Chart:       "openstack-cloud-controller-manager",
		Version:     chartVersion,
		RepoURL:     "https://kubernetes.github.io/cloud-provider-openstack",
		Values: map[string]interface{}{
			"image": map[string]interface{}{
				"repository": prefix,
				"tag":        imageTag,
			},
			"secret": map[string]interface{}{
				"create": true,
				"name":   "cloud-config-occm",
			},
			"cloudConfig": map[string]interface{}{
				"global": map[string]interface{}{
					"auth-url": cfg.Shared.AuthURL,
					"user-id":  cfg.Shared.TrusteeUserID,
					"password": cfg.Shared.TrusteePassword,
					"trust-id": cfg.Shared.TrustID,
					"region":   cfg.Shared.RegionName,
					"ca-file":  "/etc/kubernetes/ca-bundle.crt",
				},
			},
			"enabledControllers": []interface{}{
				"cloud-node",
				"cloud-node-lifecycle",
				"service",
			},
			"nodeSelector": map[string]interface{}{
				"node-role.kubernetes.io/" + roleName: "",
			},
			"tolerations": []interface{}{
				map[string]interface{}{
					"key":    "node.cloudprovider.kubernetes.io/uninitialized",
					"value":  "true",
					"effect": "NoSchedule",
				},
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
			"extraVolumes": []interface{}{
				map[string]interface{}{
					"name": "flexvolume-dir",
					"hostPath": map[string]interface{}{
						"path": "/var/lib/kubelet/volumeplugins",
					},
				},
				map[string]interface{}{
					"name": "k8s-certs",
					"hostPath": map[string]interface{}{
						"path": "/etc/kubernetes",
					},
				},
			},
			"extraVolumeMounts": []interface{}{
				map[string]interface{}{
					"name":      "flexvolume-dir",
					"mountPath": "/var/lib/kubelet/volumeplugins",
					"readOnly":  true,
				},
				map[string]interface{}{
					"name":      "k8s-certs",
					"mountPath": "/etc/kubernetes",
					"readOnly":  true,
				},
			},
			"controllerExtraArgs": "- --use-service-account-credentials=true",
			"cluster": map[string]interface{}{
				"name": cfg.Shared.ClusterUUID,
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"cloudProviderEnabled": pulumi.Bool(true),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

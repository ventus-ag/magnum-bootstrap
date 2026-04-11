package clustercindercsi

import (
	"context"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	clusterhelm "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

// cinderCSIChartVersions maps Kubernetes minor version to the matching
// openstack-cinder-csi Helm chart version.
// Update: https://github.com/kubernetes/cloud-provider-openstack/releases
var cinderCSIChartVersions = map[string]string{
	"1.35": "2.35.0",
	"1.34": "2.34.3",
	"1.33": "2.33.1",
	"1.32": "2.32.2",
	"1.31": "2.31.7",
	"1.30": "2.30.3",
	"1.29": "2.29.2",
	"1.28": "2.28.3",
	"1.27": "2.27.3",
	"1.26": "2.26.0",
	"1.25": "2.25.1",
	"1.24": "2.24.0",
}

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "cluster-cinder-csi" }
func (Module) Dependencies() []string { return []string{"cluster-rbac"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	enabled := cfg.Shared.VolumeDriver == "cinder" && cfg.Shared.CinderCSIEnabled
	if !cfg.IsFirstMaster() || !enabled {
		return clusterhelm.SkipResult()
	}
	if req.Apply {
		executor := host.NewExecutor(req.Apply, req.Logger)
		clusterhelm.AdoptHelmRelease(executor, "cinder-csi", "kube-system")
		clusterhelm.CleanupFailedRelease(executor, "cinder-csi", "kube-system")

		// Write cloud.conf to host so chart can mount it via hostPath
		// (works with both chart 2.24.x and 2.35.x).
		cloudConf := "[Global]\nauth-url=" + cfg.Shared.AuthURL +
			"\nuser-id=" + cfg.Shared.TrusteeUserID +
			"\npassword=" + cfg.Shared.TrusteePassword +
			"\ntrust-id=" + cfg.Shared.TrustID +
			"\nregion=" + cfg.Shared.RegionName +
			"\nca-file=/etc/kubernetes/ca-bundle.crt\n"
		_, _ = executor.EnsureFile("/etc/kubernetes/cloud.conf", []byte(cloudConf), 0o600)
	}
	return moduleapi.Result{}, nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	enabled := cfg.Shared.VolumeDriver == "cinder" && cfg.Shared.CinderCSIEnabled
	if !cfg.IsFirstMaster() || !enabled {
		return clusterhelm.RegisterSkipped(ctx, "magnum:cluster:CinderCSI", name, opts...)
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:CinderCSI", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	roleName := cfg.Shared.LeadNodeRoleName
	if roleName == "" {
		roleName = "master"
	}

	csiPrefix := cfg.Shared.ContainerInfraPrefix
	if csiPrefix == "" {
		csiPrefix = "registry.k8s.io/sig-storage/"
	}
	pluginPrefix := cfg.Shared.ContainerInfraPrefix
	if pluginPrefix == "" {
		pluginPrefix = "registry.k8s.io/provider-os/"
	}

	chartVersion := config.LookupByKubeVersion(cinderCSIChartVersions, cfg.Shared.KubeVersion)

	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-chart", clusterhelm.HelmReleaseArgs{
		ReleaseName: "cinder-csi",
		Namespace:   "kube-system",
		Chart:       "openstack-cinder-csi",
		Version:     chartVersion,
		RepoURL:     "https://kubernetes.github.io/cloud-provider-openstack",
		Values: map[string]interface{}{
			"csi": map[string]interface{}{
				"attacher": map[string]interface{}{
					"image": map[string]interface{}{
						"repository": csiPrefix + "csi-attacher",
					},
				},
				"provisioner": map[string]interface{}{
					"topology": "true",
					"image": map[string]interface{}{
						"repository": csiPrefix + "csi-provisioner",
					},
				},
				"snapshotter": map[string]interface{}{
					"image": map[string]interface{}{
						"repository": csiPrefix + "csi-snapshotter",
					},
				},
				"resizer": map[string]interface{}{
					"image": map[string]interface{}{
						"repository": csiPrefix + "csi-resizer",
					},
				},
				"livenessprobe": map[string]interface{}{
					"image": map[string]interface{}{
						"repository": csiPrefix + "livenessprobe",
					},
				},
				"nodeDriverRegistrar": map[string]interface{}{
					"image": map[string]interface{}{
						"repository": csiPrefix + "csi-node-driver-registrar",
					},
				},
				"plugin": map[string]interface{}{
					"image": map[string]interface{}{
						"repository": pluginPrefix + "cinder-csi-plugin",
					},
					"volumes": []interface{}{
						map[string]interface{}{
							"name": "cacert",
							"hostPath": map[string]interface{}{
								"path": "/etc/kubernetes/ca-bundle.crt",
								"type": "File",
							},
						},
					},
					"volumeMounts": []interface{}{
						map[string]interface{}{
							"name":      "cacert",
							"mountPath": "/etc/kubernetes/certs/ca-bundle.crt",
							"readOnly":  true,
						},
						map[string]interface{}{
							"name":      "cloud-config",
							"mountPath": "/etc/kubernetes/config/",
							"readOnly":  true,
						},
					},
					"controllerPlugin": map[string]interface{}{
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
					},
					"nodePlugin": map[string]interface{}{
						"affinity":     map[string]interface{}{},
						"nodeSelector": map[string]interface{}{},
						"tolerations": []interface{}{
							map[string]interface{}{
								"operator": "Exists",
							},
						},
						"kubeletDir": "/var/lib/kubelet",
					},
				},
			},
			"secret": map[string]interface{}{
				"enabled": false,
			},
			"storageClass": map[string]interface{}{
				"enabled": true,
				"delete": map[string]interface{}{
					"isDefault":            true,
					"allowVolumeExpansion": true,
				},
				"retain": map[string]interface{}{
					"isDefault":            false,
					"allowVolumeExpansion": true,
				},
			},
			"clusterID":         cfg.Shared.ClusterUUID,
			"priorityClassName": "",
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"cinderCSIEnabled": pulumi.Bool(true),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

package clustermanilacsi

import (
	"context"

	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	storagev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/storage/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	clusterhelm "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

// manilaCSIChartVersions maps K8s minor version to the Manila CSI Helm chart version.
// Source: helm search repo cpo/openstack-manila-csi --versions
var manilaCSIChartVersions = map[string]string{
	"1.35": "2.35.0",
	"1.34": "2.34.2",
	"1.33": "2.33.1",
	"1.32": "2.32.0",
	"1.31": "2.31.5",
	"1.30": "2.30.3",
	"1.29": "2.29.0",
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

func (Module) PhaseID() string        { return "cluster-manila-csi" }
func (Module) Dependencies() []string { return []string{"cluster-cleanup-deprecated"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if !cfg.IsFirstMaster() || !cfg.Shared.ManilaCSIEnabled {
		return clusterhelm.SkipResult()
	}
	if req.Apply {
		executor := host.NewExecutor(req.Apply, req.Logger)
		clusterhelm.AdoptHelmRelease(executor, "nfs-driver", "kube-system")
		clusterhelm.AdoptHelmRelease(executor, "openstack-manila-csi", "kube-system")
	}
	return moduleapi.Result{
		Outputs: map[string]string{"firstMaster": "true"},
	}, nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	if !cfg.IsFirstMaster() || !cfg.Shared.ManilaCSIEnabled {
		return clusterhelm.RegisterSkipped(ctx, "magnum:cluster:ManilaCSI", name, opts...)
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:ManilaCSI", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	nfsChartVersion := "v4.9.0"

	// NFS CSI driver (dependency for Manila).
	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-nfs-driver", clusterhelm.HelmReleaseArgs{
		ReleaseName: "nfs-driver",
		Namespace:   "kube-system",
		Chart:       "csi-driver-nfs",
		Version:     nfsChartVersion,
		RepoURL:     "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/master/charts",
		Values:      nfsDriverValues(cfg),
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	manilaChartVersion := config.LookupByKubeVersion(manilaCSIChartVersions, cfg.Shared.KubeVersion)
	clusterhelm.WarnIfClampedBelow(ctx, "cluster-manila-csi", manilaCSIChartVersions, cfg.Shared.KubeVersion)

	// Manila CSI plugin via Helm.
	_, err = clusterhelm.DeployHelmRelease(ctx, name+"-manila-plugin", clusterhelm.HelmReleaseArgs{
		ReleaseName: "openstack-manila-csi",
		Namespace:   "kube-system",
		Chart:       "openstack-manila-csi",
		Version:     manilaChartVersion,
		RepoURL:     "https://kubernetes.github.io/cloud-provider-openstack",
		Values:      manilaPluginValues(cfg),
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	// Manila CSI secrets.
	_, err = corev1.NewSecret(ctx, name+"-manila-secrets", &corev1.SecretArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String("csi-manila-secrets"),
			Namespace: pulumi.String("kube-system"),
			Annotations: pulumi.StringMap{
				"pulumi.com/patchForce": pulumi.String("true"),
				"pulumi.com/skipAwait":  pulumi.String("true"),
			},
		},
		StringData: pulumi.StringMap{
			"os-authURL":         pulumi.String(cfg.Shared.AuthURL),
			"os-region":          pulumi.String(cfg.Shared.RegionName),
			"os-trustID":         pulumi.String(cfg.Shared.TrustID),
			"os-trusteeID":       pulumi.String(cfg.Shared.TrusteeUserID),
			"os-trusteePassword": pulumi.String(cfg.Shared.TrusteePassword),
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	// StorageClass: csi-manila-nfs
	_, err = storagev1.NewStorageClass(ctx, name+"-storageclass", &storagev1.StorageClassArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name: pulumi.String("csi-manila-nfs"),
			Annotations: pulumi.StringMap{
				"pulumi.com/patchForce": pulumi.String("true"),
				"pulumi.com/skipAwait":  pulumi.String("true"),
			},
		},
		Provisioner: pulumi.String("nfs.manila.csi.openstack.org"),
		Parameters: pulumi.StringMap{
			"type": pulumi.String(manilaShareType(cfg)),
			"csi.storage.k8s.io/provisioner-secret-name":       pulumi.String("csi-manila-secrets"),
			"csi.storage.k8s.io/provisioner-secret-namespace":  pulumi.String("kube-system"),
			"csi.storage.k8s.io/node-stage-secret-name":        pulumi.String("csi-manila-secrets"),
			"csi.storage.k8s.io/node-stage-secret-namespace":   pulumi.String("kube-system"),
			"csi.storage.k8s.io/node-publish-secret-name":      pulumi.String("csi-manila-secrets"),
			"csi.storage.k8s.io/node-publish-secret-namespace": pulumi.String("kube-system"),
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"manilaCSIEnabled": pulumi.Bool(true),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

// nfsDriverValues builds csi-driver-nfs chart values. CONTAINER_INFRA_PREFIX
// redirects the sig-storage sidecar/driver images to the mirror (tags stay
// chart defaults), consistent with cinder-csi.
func nfsDriverValues(cfg config.Config) map[string]interface{} {
	values := map[string]interface{}{
		"controller": map[string]interface{}{
			"replicas": 2,
		},
	}
	if prefix := cfg.Shared.ContainerInfraPrefix; prefix != "" {
		values["image"] = map[string]interface{}{
			"nfs":                 map[string]interface{}{"repository": prefix + "nfsplugin"},
			"csiProvisioner":      map[string]interface{}{"repository": prefix + "csi-provisioner"},
			"csiResizer":          map[string]interface{}{"repository": prefix + "csi-resizer"},
			"csiSnapshotter":      map[string]interface{}{"repository": prefix + "csi-snapshotter"},
			"livenessProbe":       map[string]interface{}{"repository": prefix + "livenessprobe"},
			"nodeDriverRegistrar": map[string]interface{}{"repository": prefix + "csi-node-driver-registrar"},
		}
	}
	return values
}

// manilaPluginValues builds openstack-manila-csi chart values with the same
// CONTAINER_INFRA_PREFIX handling.
func manilaPluginValues(cfg config.Config) map[string]interface{} {
	values := map[string]interface{}{
		"fullnameOverride": "",
		"shareProtocols": []interface{}{
			map[string]interface{}{
				"protocolSelector": "NFS",
				"fsGroupPolicy":    "None",
				"fwdNodePluginEndpoint": map[string]interface{}{
					"dir":      "/var/lib/kubelet/plugins/csi-nfsplugin",
					"sockFile": "csi.sock",
				},
			},
		},
	}
	if prefix := cfg.Shared.ContainerInfraPrefix; prefix != "" {
		values["csimanila"] = map[string]interface{}{
			"image": map[string]interface{}{
				"repository": prefix + "manila-csi-plugin",
			},
		}
		registrar := map[string]interface{}{
			"registrar": map[string]interface{}{
				"image": map[string]interface{}{
					"repository": prefix + "csi-node-driver-registrar",
				},
			},
		}
		values["nodeplugin"] = registrar
		values["controllerplugin"] = map[string]interface{}{
			"provisioner": map[string]interface{}{
				"image": map[string]interface{}{
					"repository": prefix + "csi-provisioner",
				},
			},
			"snapshotter": map[string]interface{}{
				"image": map[string]interface{}{
					"repository": prefix + "csi-snapshotter",
				},
			},
			"resizer": map[string]interface{}{
				"image": map[string]interface{}{
					"repository": prefix + "csi-resizer",
				},
			},
		}
	}
	return values
}

// manilaShareType resolves the Manila share type for the StorageClass. The
// default preserves the ventus cloud's value; other deployments set the
// MANILA_SHARE_TYPE heat-param.
func manilaShareType(cfg config.Config) string {
	if t := cfg.Shared.ManilaShareType; t != "" {
		return t
	}
	return "cephfsnfs1"
}

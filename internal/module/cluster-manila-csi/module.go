package clustermanilacsi

import (
	"context"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	storagev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/storage/v1"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	clusterhelm "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "cluster-manila-csi" }
func (Module) Dependencies() []string { return []string{"cluster-rbac"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	return clusterhelm.RunNoop(ctx, cfg, req, cfg.Shared.ManilaCSIPluginEnabled, "openstack-manila-csi", "kube-system")
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	if !cfg.IsFirstMaster() || !cfg.Shared.ManilaCSIPluginEnabled {
		return clusterhelm.RegisterSkipped(ctx, "magnum:cluster:ManilaCSI", name, opts...)
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:ManilaCSI", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	// NFS CSI driver (dependency for Manila).
	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-nfs-driver", clusterhelm.HelmReleaseArgs{
		ReleaseName: "nfs-driver",
		Namespace:   "kube-system",
		Chart:       "csi-driver-nfs",
		Version:     "4.5.0",
		RepoURL:     "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/master/charts",
		Values: map[string]interface{}{
			"controller": map[string]interface{}{
				"replicas": 2,
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	// Manila CSI plugin via Helm.
	_, err = clusterhelm.DeployHelmRelease(ctx, name+"-manila-plugin", clusterhelm.HelmReleaseArgs{
		ReleaseName: "openstack-manila-csi",
		Namespace:   "kube-system",
		Chart:       "openstack-manila-csi",
		Version:     "2.27.1",
		RepoURL:     "https://kubernetes.github.io/cloud-provider-openstack",
		Values: map[string]interface{}{
			"fullnameOverride": "",
			"shareProtocols": []interface{}{
				map[string]interface{}{
					"protocolSelector": "NFS",
					"fwdNodePluginEndpoint": map[string]interface{}{
						"dir":      "/var/lib/kubelet/plugins/csi-nfsplugin",
						"sockFile": "csi.sock",
					},
				},
			},
		},
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
			},
		},
		Provisioner: pulumi.String("nfs.manila.csi.openstack.org"),
		Parameters: pulumi.StringMap{
			"type": pulumi.String("cephfsnfs1"),
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

package clusterautohealer

import (
	"context"
	"fmt"

	appsv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apps/v1"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	rbacv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/rbac/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	clusterhelm "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "cluster-auto-healer" }
func (Module) Dependencies() []string { return []string{"cluster-rbac"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	// NPD and auto-healer are both managed via Register().
	// Adopt NPD Helm release if it was installed by legacy bash.
	if cfg.IsFirstMaster() && cfg.Shared.NPDEnabled {
		if req.Apply {
			executor := host.NewExecutor(req.Apply, req.Logger)
			clusterhelm.AdoptHelmRelease(executor, "npd", "kube-system")
		}
	}
	return moduleapi.Result{}, nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg

	// Skip entirely if not first master and neither feature is enabled.
	if !cfg.IsFirstMaster() || (!cfg.Shared.NPDEnabled && !cfg.Shared.AutoHealingEnabled) {
		return clusterhelm.RegisterSkipped(ctx, "magnum:cluster:AutoHealer", name, opts...)
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:AutoHealer", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	// --- NPD (Node Problem Detector) via Helm ---
	if cfg.Shared.NPDEnabled {
		if err := registerNPD(ctx, name, cfg, childOpts); err != nil {
			return nil, err
		}
	}

	// --- magnum-auto-healer DaemonSet ---
	if cfg.Shared.AutoHealingEnabled {
		if err := registerAutoHealer(ctx, name, cfg, childOpts); err != nil {
			return nil, err
		}
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"npdEnabled":         pulumi.Bool(cfg.Shared.NPDEnabled),
		"autoHealingEnabled": pulumi.Bool(cfg.Shared.AutoHealingEnabled),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

func registerNPD(ctx *pulumi.Context, name string, cfg config.Config, opts []pulumi.ResourceOption) error {
	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/"
	}

	chartVersion := cfg.Shared.NPDChartTag
	if chartVersion == "" {
		chartVersion = "2.3.4"
	}

	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-npd", clusterhelm.HelmReleaseArgs{
		ReleaseName: "npd",
		Namespace:   "kube-system",
		Chart:       "node-problem-detector",
		Version:     chartVersion,
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
			"updateStrategy": "RollingUpdate",
			"maxUnavailable": 1,
		},
	}, opts...)
	return err
}

func registerAutoHealer(ctx *pulumi.Context, name string, cfg config.Config, opts []pulumi.ResourceOption) error {
	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/"
	}

	roleName := cfg.Shared.LeadNodeRoleName
	if roleName == "" {
		roleName = "master"
	}

	patchMeta := func(n, ns string) *metav1.ObjectMetaArgs {
		return &metav1.ObjectMetaArgs{
			Name:      pulumi.String(n),
			Namespace: pulumi.String(ns),
			Annotations: pulumi.StringMap{
				"pulumi.com/patchForce": pulumi.String("true"),
				"pulumi.com/skipAwait":  pulumi.String("true"),
			},
		}
	}

	// ServiceAccount
	_, err := corev1.NewServiceAccount(ctx, name+"-autohealer-sa", &corev1.ServiceAccountArgs{
		Metadata: patchMeta("magnum-auto-healer", "kube-system"),
	}, opts...)
	if err != nil {
		return err
	}

	// ClusterRoleBinding
	_, err = rbacv1.NewClusterRoleBinding(ctx, name+"-autohealer-crb", &rbacv1.ClusterRoleBindingArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name: pulumi.String("magnum-auto-healer"),
			Annotations: pulumi.StringMap{
				"pulumi.com/patchForce": pulumi.String("true"),
				"pulumi.com/skipAwait":  pulumi.String("true"),
			},
		},
		RoleRef: &rbacv1.RoleRefArgs{
			ApiGroup: pulumi.String("rbac.authorization.k8s.io"),
			Kind:     pulumi.String("ClusterRole"),
			Name:     pulumi.String("cluster-admin"),
		},
		Subjects: rbacv1.SubjectArray{
			&rbacv1.SubjectArgs{
				Kind:      pulumi.String("ServiceAccount"),
				Name:      pulumi.String("magnum-auto-healer"),
				Namespace: pulumi.String("kube-system"),
			},
		},
	}, opts...)
	if err != nil {
		return err
	}

	// ConfigMap with auto-healer config
	configYAML := fmt.Sprintf(`cluster-name: %s
dry-run: false
monitor-interval: 15s
check-delay-after-add: 20m
leader-elect: true
healthcheck:
  master:
    - type: Endpoint
      params:
        unhealthy-duration: 30s
        protocol: HTTPS
        port: 6443
        endpoints: ["/healthz"]
        ok-codes: [200]
    - type: NodeCondition
      params:
        unhealthy-duration: 1m
        types: ["Ready"]
        ok-values: ["True"]
  worker:
    - type: NodeCondition
      params:
        unhealthy-duration: 1m
        types: ["Ready"]
        ok-values: ["True"]
openstack:
  auth-url: %s
  user-id: %s
  password: %s
  trust-id: %s
  region: %s
  ca-file: /etc/kubernetes/ca-bundle.crt`,
		cfg.Shared.ClusterUUID,
		cfg.Shared.AuthURL,
		cfg.Shared.TrusteeUserID,
		cfg.Shared.TrusteePassword,
		cfg.Shared.TrustID,
		cfg.Shared.RegionName,
	)

	_, err = corev1.NewConfigMap(ctx, name+"-autohealer-cm", &corev1.ConfigMapArgs{
		Metadata: patchMeta("magnum-auto-healer-config", "kube-system"),
		Data: pulumi.StringMap{
			"config.yaml": pulumi.String(configYAML),
		},
	}, opts...)
	if err != nil {
		return err
	}

	// DaemonSet
	_, err = appsv1.NewDaemonSet(ctx, name+"-autohealer-ds", &appsv1.DaemonSetArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String("magnum-auto-healer"),
			Namespace: pulumi.String("kube-system"),
			Labels: pulumi.StringMap{
				"k8s-app": pulumi.String("magnum-auto-healer"),
			},
			Annotations: pulumi.StringMap{
				"pulumi.com/patchForce": pulumi.String("true"),
				"pulumi.com/skipAwait":  pulumi.String("true"),
			},
		},
		Spec: &appsv1.DaemonSetSpecArgs{
			Selector: &metav1.LabelSelectorArgs{
				MatchLabels: pulumi.StringMap{
					"k8s-app": pulumi.String("magnum-auto-healer"),
				},
			},
			Template: &corev1.PodTemplateSpecArgs{
				Metadata: &metav1.ObjectMetaArgs{
					Labels: pulumi.StringMap{
						"k8s-app": pulumi.String("magnum-auto-healer"),
					},
				},
				Spec: &corev1.PodSpecArgs{
					ServiceAccountName: pulumi.String("magnum-auto-healer"),
					DnsPolicy:          pulumi.String("Default"),
					NodeSelector: pulumi.StringMap{
						"node-role.kubernetes.io/" + roleName: pulumi.String(""),
					},
					Tolerations: corev1.TolerationArray{
						&corev1.TolerationArgs{
							Effect:   pulumi.String("NoSchedule"),
							Operator: pulumi.String("Exists"),
						},
						&corev1.TolerationArgs{
							Key:      pulumi.String("CriticalAddonsOnly"),
							Operator: pulumi.String("Exists"),
						},
						&corev1.TolerationArgs{
							Effect:   pulumi.String("NoExecute"),
							Operator: pulumi.String("Exists"),
						},
					},
					Containers: corev1.ContainerArray{
						&corev1.ContainerArgs{
							Name:            pulumi.String("magnum-auto-healer"),
							Image:           pulumi.String(prefix + "provider-os/magnum-auto-healer:v1.27.1"),
							ImagePullPolicy: pulumi.String("Always"),
							Args: pulumi.StringArray{
								pulumi.String("/bin/magnum-auto-healer"),
								pulumi.String("--config=/etc/magnum-auto-healer/config.yaml"),
								pulumi.String("--v"),
								pulumi.String("2"),
							},
							VolumeMounts: corev1.VolumeMountArray{
								&corev1.VolumeMountArgs{
									Name:      pulumi.String("config"),
									MountPath: pulumi.String("/etc/magnum-auto-healer"),
								},
								&corev1.VolumeMountArgs{
									Name:      pulumi.String("kubernetes-config"),
									MountPath: pulumi.String("/etc/kubernetes"),
									ReadOnly:  pulumi.Bool(true),
								},
							},
						},
					},
					Volumes: corev1.VolumeArray{
						&corev1.VolumeArgs{
							Name: pulumi.String("config"),
							ConfigMap: &corev1.ConfigMapVolumeSourceArgs{
								Name: pulumi.String("magnum-auto-healer-config"),
							},
						},
						&corev1.VolumeArgs{
							Name: pulumi.String("kubernetes-config"),
							HostPath: &corev1.HostPathVolumeSourceArgs{
								Path: pulumi.String("/etc/kubernetes"),
							},
						},
					},
				},
			},
		},
	}, opts...)
	return err
}

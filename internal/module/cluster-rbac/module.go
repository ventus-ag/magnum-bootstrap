package clusterrbac

import (
	"context"
	"encoding/base64"
	"os"

	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	rbacv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/rbac/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

// mergeMetadata creates ObjectMetaArgs with patchForce annotation to take
// ownership of pre-existing resources created by kubectl. Without this,
// server-side apply fails with field manager conflicts.
func mergeMetadata(name string, ns string) *metav1.ObjectMetaArgs {
	meta := &metav1.ObjectMetaArgs{
		Name: pulumi.String(name),
		Annotations: pulumi.StringMap{
			"pulumi.com/patchForce": pulumi.String("true"),
			"pulumi.com/skipAwait":  pulumi.String("true"),
		},
	}
	if ns != "" {
		meta.Namespace = pulumi.String(ns)
	}
	return meta
}

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "cluster-rbac" }
func (Module) Dependencies() []string { return []string{"health"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if !cfg.IsFirstMaster() {
		return moduleapi.Result{}, nil
	}

	// Label kube-system namespace with Pod Security Admission "privileged"
	// enforcement. K8s 1.25+ enables PSA by default — without this,
	// infrastructure DaemonSets (CSI, OCCM) that need privileged containers
	// are rejected. Applied imperatively here to guarantee it runs before
	// any Helm releases in Register(). Also set via Pulumi in Register()
	// as a reliable backup.
	executor := host.NewExecutor(req.Apply, req.Logger)
	if err := executor.Run("kubectl", "--kubeconfig=/etc/kubernetes/admin.conf",
		"label", "namespace", "kube-system",
		"pod-security.kubernetes.io/enforce=privileged",
		"pod-security.kubernetes.io/audit=privileged",
		"pod-security.kubernetes.io/warn=privileged",
		"--overwrite"); err != nil {
		if req.Logger != nil {
			req.Logger.Warnf("cluster-rbac: failed to label kube-system with PSA labels: %v", err)
		}
	}

	return moduleapi.Result{
		Outputs: map[string]string{"firstMaster": "true"},
	}, nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	if !cfg.IsFirstMaster() {
		return registerEmpty(ctx, name, opts...)
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:RBAC", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	// Patch kube-system namespace with PSA "privileged" labels.
	// This is the authoritative source — the kubectl in Run() is a
	// best-effort early attempt but can fail on fresh clusters.
	// All downstream Helm releases depend on cluster-rbac, so these
	// labels are guaranteed to be set before any addon DaemonSets.
	_, err := corev1.NewNamespacePatch(ctx, name+"-kube-system-psa", &corev1.NamespacePatchArgs{
		Metadata: &metav1.ObjectMetaPatchArgs{
			Name: pulumi.String("kube-system"),
			Labels: pulumi.StringMap{
				"pod-security.kubernetes.io/enforce": pulumi.String("privileged"),
				"pod-security.kubernetes.io/audit":   pulumi.String("privileged"),
				"pod-security.kubernetes.io/warn":    pulumi.String("privileged"),
			},
			Annotations: pulumi.StringMap{
				"pulumi.com/patchForce": pulumi.String("true"),
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	// ClusterRole: system:kube-apiserver-to-kubelet
	_, err = rbacv1.NewClusterRole(ctx, name+"-apiserver-kubelet", &rbacv1.ClusterRoleArgs{
		Metadata: mergeMetadata("system:kube-apiserver-to-kubelet", ""),
		Rules: rbacv1.PolicyRuleArray{
			&rbacv1.PolicyRuleArgs{
				ApiGroups: pulumi.StringArray{pulumi.String("")},
				Resources: pulumi.StringArray{
					pulumi.String("nodes/proxy"),
					pulumi.String("nodes/stats"),
					pulumi.String("nodes/log"),
					pulumi.String("nodes/spec"),
					pulumi.String("nodes/metrics"),
				},
				Verbs: pulumi.StringArray{pulumi.String("*")},
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	// ClusterRoleBinding: system:kube-apiserver
	_, err = rbacv1.NewClusterRoleBinding(ctx, name+"-apiserver-binding", &rbacv1.ClusterRoleBindingArgs{
		Metadata: mergeMetadata("system:kube-apiserver", ""),
		RoleRef: &rbacv1.RoleRefArgs{
			ApiGroup: pulumi.String("rbac.authorization.k8s.io"),
			Kind:     pulumi.String("ClusterRole"),
			Name:     pulumi.String("system:kube-apiserver-to-kubelet"),
		},
		Subjects: rbacv1.SubjectArray{
			&rbacv1.SubjectArgs{
				ApiGroup: pulumi.String("rbac.authorization.k8s.io"),
				Kind:     pulumi.String("User"),
				Name:     pulumi.String("kubernetes"),
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	// ServiceAccount: admin in kube-system
	_, err = corev1.NewServiceAccount(ctx, name+"-admin-sa", &corev1.ServiceAccountArgs{
		Metadata: mergeMetadata("admin", "kube-system"),
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	// ClusterRoleBinding: admin -> cluster-admin
	_, err = rbacv1.NewClusterRoleBinding(ctx, name+"-admin-binding", &rbacv1.ClusterRoleBindingArgs{
		Metadata: mergeMetadata("admin", ""),
		RoleRef: &rbacv1.RoleRefArgs{
			ApiGroup: pulumi.String("rbac.authorization.k8s.io"),
			Kind:     pulumi.String("ClusterRole"),
			Name:     pulumi.String("cluster-admin"),
		},
		Subjects: rbacv1.SubjectArray{
			&rbacv1.SubjectArgs{
				Kind:      pulumi.String("ServiceAccount"),
				Name:      pulumi.String("admin"),
				Namespace: pulumi.String("kube-system"),
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	// ClusterRole: system:node-drainer
	_, err = rbacv1.NewClusterRole(ctx, name+"-node-drainer", &rbacv1.ClusterRoleArgs{
		Metadata: mergeMetadata("system:node-drainer", ""),
		Rules: rbacv1.PolicyRuleArray{
			&rbacv1.PolicyRuleArgs{
				ApiGroups: pulumi.StringArray{pulumi.String("")},
				Resources: pulumi.StringArray{pulumi.String("pods/eviction")},
				Verbs:     pulumi.StringArray{pulumi.String("create")},
			},
			&rbacv1.PolicyRuleArgs{
				ApiGroups: pulumi.StringArray{pulumi.String("apps"), pulumi.String("extensions")},
				Resources: pulumi.StringArray{pulumi.String("statefulsets"), pulumi.String("daemonsets")},
				Verbs:     pulumi.StringArray{pulumi.String("get"), pulumi.String("list")},
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	// ClusterRoleBinding: system:node-drainer -> system:nodes
	_, err = rbacv1.NewClusterRoleBinding(ctx, name+"-node-drainer-binding", &rbacv1.ClusterRoleBindingArgs{
		Metadata: mergeMetadata("system:node-drainer", ""),
		RoleRef: &rbacv1.RoleRefArgs{
			ApiGroup: pulumi.String("rbac.authorization.k8s.io"),
			Kind:     pulumi.String("ClusterRole"),
			Name:     pulumi.String("system:node-drainer"),
		},
		Subjects: rbacv1.SubjectArray{
			&rbacv1.SubjectArgs{
				ApiGroup: pulumi.String("rbac.authorization.k8s.io"),
				Kind:     pulumi.String("Group"),
				Name:     pulumi.String("system:nodes"),
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	// ClusterRole: system:pod-interactive-access
	// K8s 1.35+ requires CREATE verb on pods/exec, pods/attach, pods/portforward
	// subresources for WebSocket-based streaming operations.
	_, err = rbacv1.NewClusterRole(ctx, name+"-pod-interactive", &rbacv1.ClusterRoleArgs{
		Metadata: mergeMetadata("system:pod-interactive-access", ""),
		Rules: rbacv1.PolicyRuleArray{
			&rbacv1.PolicyRuleArgs{
				ApiGroups: pulumi.StringArray{pulumi.String("")},
				Resources: pulumi.StringArray{
					pulumi.String("pods/exec"),
					pulumi.String("pods/attach"),
					pulumi.String("pods/portforward"),
				},
				Verbs: pulumi.StringArray{
					pulumi.String("create"),
					pulumi.String("get"),
				},
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	// ClusterRoleBinding: system:pod-interactive-access -> system:masters + system:nodes
	_, err = rbacv1.NewClusterRoleBinding(ctx, name+"-pod-interactive-binding", &rbacv1.ClusterRoleBindingArgs{
		Metadata: mergeMetadata("system:pod-interactive-access", ""),
		RoleRef: &rbacv1.RoleRefArgs{
			ApiGroup: pulumi.String("rbac.authorization.k8s.io"),
			Kind:     pulumi.String("ClusterRole"),
			Name:     pulumi.String("system:pod-interactive-access"),
		},
		Subjects: rbacv1.SubjectArray{
			&rbacv1.SubjectArgs{
				ApiGroup: pulumi.String("rbac.authorization.k8s.io"),
				Kind:     pulumi.String("Group"),
				Name:     pulumi.String("system:masters"),
			},
			&rbacv1.SubjectArgs{
				ApiGroup: pulumi.String("rbac.authorization.k8s.io"),
				Kind:     pulumi.String("Group"),
				Name:     pulumi.String("system:nodes"),
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	// Secret: os-trustee
	caBundle := ""
	if data, err := os.ReadFile("/etc/kubernetes/ca-bundle.crt"); err == nil {
		caBundle = base64.StdEncoding.EncodeToString(data)
	} else if data, err := os.ReadFile("/etc/kubernetes/certs/ca.crt"); err == nil {
		// Fallback: use the cluster CA cert if the bundle is not yet available.
		caBundle = base64.StdEncoding.EncodeToString(data)
	}
	// If both paths fail, continue with empty CA bundle — certs may not be
	// generated yet during early cluster creation.
	_, err = corev1.NewSecret(ctx, name+"-os-trustee", &corev1.SecretArgs{
		Metadata: mergeMetadata("os-trustee", "kube-system"),
		StringData: pulumi.StringMap{
			"os-authURL":         pulumi.String(cfg.Shared.AuthURL),
			"os-trustID":         pulumi.String(cfg.Shared.TrustID),
			"os-trusteeID":       pulumi.String(cfg.Shared.TrusteeUserID),
			"os-trusteePassword": pulumi.String(cfg.Shared.TrusteePassword),
			"os-region":          pulumi.String(cfg.Shared.RegionName),
			"os-certAuthority":   pulumi.String(caBundle),
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"clusterUuid": pulumi.String(cfg.Shared.ClusterUUID),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

func registerEmpty(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:RBAC", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"skipped": pulumi.Bool(true),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

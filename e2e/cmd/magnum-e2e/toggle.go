package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/clusters"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// addonToggle describes a cluster addon that the fork enables/disables via a
// cluster label. Flipping the label triggers a Magnum reconfigure_cluster (a
// labels-only stack update that re-fires the master reconciler run-once); the
// reconciler's cluster-* module then either deploys the addon's Helm release or,
// when the flag is off, leaves it out of the Pulumi program so `up` prunes
// (uninstalls) it. The Deployment is the observable end state.
type addonToggle struct {
	label      string // cluster label key, e.g. "auto_scaling_enabled"
	namespace  string
	deployment string // Deployment the addon ultimately creates
}

var (
	autoscalerToggle = addonToggle{
		label:      "auto_scaling_enabled",
		namespace:  autoscalerNS,     // "kube-system"
		deployment: autoscalerDeploy, // "openstack-autoscaler-manager"
	}
	metricsServerToggle = addonToggle{
		label:      "metrics_server_enabled",
		namespace:  "kube-system",
		deployment: "metrics-server",
	}
)

// setCreateLabel sets a create-time cluster label in cfg.extraLabels, replacing
// any existing value for the key (last write wins in buildLabels, but we keep the
// string clean). Used to pin a toggle op's starting state at create.
func (r *runner) setCreateLabel(key, val string) {
	kept := []string{}
	for kv := range strings.SplitSeq(r.cfg.extraLabels, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		if k, _, ok := strings.Cut(kv, "="); ok && strings.TrimSpace(k) == key {
			continue // drop the prior value for this key
		}
		kept = append(kept, kv)
	}
	kept = append(kept, key+"="+val)
	r.cfg.extraLabels = strings.Join(kept, ",")
	r.log("toggle: pinning create label %s=%s", key, val)
}

// toggleAddon flips the addon's cluster label on the live cluster and asserts the
// reconciler converged: enable ⇒ Deployment becomes Ready; disable ⇒ Deployment
// is removed (Helm release pruned). The PATCH runs through runMutation so it gets
// the settle → trigger(retry) → wait UPDATE_COMPLETE → verifyBundle wrapper; the
// addon-specific assertion runs after the cluster reports converged.
func (r *runner) toggleAddon(ctx context.Context, a addonToggle, enable bool) error {
	val := "false"
	verb := "disable"
	if enable {
		val = "true"
		verb = "enable"
	}
	if err := r.runMutation(ctx, fmt.Sprintf("%s %s (%s=%s)", verb, a.deployment, a.label, val), true, func() error {
		return r.patchClusterLabel(ctx, a.label, val)
	}); err != nil {
		return err
	}

	kc, err := r.k8sClient(ctx)
	if err != nil {
		return fmt.Errorf("toggle %s: k8s client: %w", a.label, err)
	}
	if enable {
		r.log("toggle: asserting %s/%s becomes Ready", a.namespace, a.deployment)
		return r.waitDeploymentReady(ctx, kc, a.namespace, a.deployment)
	}
	r.log("toggle: asserting %s/%s is removed (Helm release pruned)", a.namespace, a.deployment)
	return r.waitDeploymentAbsent(ctx, kc, a.namespace, a.deployment)
}

// patchClusterLabel PATCHes the cluster's label map (replace /labels with the
// full current set plus the one changed key — Magnum's replace op swaps the whole
// map, so we must preserve the others). This is the trigger the fork turns into a
// reconfigure_cluster stack update.
func (r *runner) patchClusterLabel(ctx context.Context, key, val string) error {
	c, err := clusters.Get(ctx, r.magnum, r.cfg.clusterName).Extract()
	if err != nil {
		return fmt.Errorf("get cluster for label patch: %w", err)
	}
	labels := make(map[string]string, len(c.Labels)+1)
	for k, v := range c.Labels {
		labels[k] = v
	}
	labels[key] = val
	opts := []clusters.UpdateOpts{{
		Op:    clusters.ReplaceOp,
		Path:  "/labels",
		Value: labels,
	}}
	if _, err := clusters.Update(ctx, r.magnum, c.UUID, opts).Extract(); err != nil {
		return fmt.Errorf("patch cluster label %s=%s: %w", key, val, err)
	}
	return nil
}

// waitDeploymentReady polls until the named Deployment has at least one available
// replica and all desired replicas ready.
func (r *runner) waitDeploymentReady(ctx context.Context, kc *kubernetes.Clientset, ns, name string) error {
	deadline := time.Now().Add(8 * time.Minute)
	for {
		d, err := kc.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			want := int32(1)
			if d.Spec.Replicas != nil {
				want = *d.Spec.Replicas
			}
			if d.Status.AvailableReplicas >= 1 && d.Status.ReadyReplicas >= want {
				r.log("toggle: %s/%s Ready (%d/%d)", ns, name, d.Status.ReadyReplicas, want)
				return nil
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get deployment %s/%s: %w", ns, name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("deployment %s/%s not Ready within timeout (enable did not install it)", ns, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// waitDeploymentAbsent polls until the named Deployment is gone (NotFound),
// proving the reconciler uninstalled the addon when its flag flipped off.
func (r *runner) waitDeploymentAbsent(ctx context.Context, kc *kubernetes.Clientset, ns, name string) error {
	deadline := time.Now().Add(8 * time.Minute)
	for {
		_, err := kc.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			r.log("toggle: %s/%s removed", ns, name)
			return nil
		}
		if err != nil {
			return fmt.Errorf("get deployment %s/%s: %w", ns, name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("deployment %s/%s still present within timeout (disable did not uninstall it)", ns, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

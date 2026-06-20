package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	autoscalerNS     = "kube-system"
	autoscalerDeploy = "openstack-autoscaler-manager" // Helm release "openstack-autoscaler" + nameOverride "manager"
	balloonDeploy    = "e2e-autoscale"
	balloonApp       = "e2e-autoscale"
)

// controlPlaneLabels mark a control-plane node. Both keys are checked because the
// label was renamed master→control-plane at k8s 1.25 (the ladder spans 1.20→1.35),
// and Magnum control-plane nodes are frequently schedulable (untainted), so a
// worker-only workload must explicitly exclude them — otherwise a balloon pod
// lands on a master and the autoscaler under-scales the worker nodegroup.
var controlPlaneLabels = []string{
	"node-role.kubernetes.io/control-plane",
	"node-role.kubernetes.io/master",
}

func isControlPlane(node *corev1.Node) bool {
	for _, l := range controlPlaneLabels {
		if _, ok := node.Labels[l]; ok {
			return true
		}
	}
	return false
}

// enableAutoscalerLabels injects the cluster labels that turn the
// cluster-autoscaler on at create (master-0 deploys it from AUTO_SCALING_ENABLED;
// the worker nodegroup's min/max come from MIN_NODE_COUNT/MAX_NODE_COUNT). Magnum
// cluster labels are lowercase. Existing user-supplied labels win.
func (r *runner) enableAutoscalerLabels() {
	existing := map[string]bool{}
	for kv := range strings.SplitSeq(r.cfg.extraLabels, ",") {
		if k, _, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
			existing[strings.TrimSpace(k)] = true
		}
	}
	add := func(k, v string) {
		if existing[k] {
			return
		}
		if r.cfg.extraLabels != "" {
			r.cfg.extraLabels += ","
		}
		r.cfg.extraLabels += k + "=" + v
	}
	add("auto_scaling_enabled", "true")
	add("min_node_count", strconv.Itoa(r.cfg.autoscaleMin))
	add("max_node_count", strconv.Itoa(r.cfg.autoscaleMax))
	r.log("autoscale: enabling cluster-autoscaler at create (min=%d max=%d) — labels: %s",
		r.cfg.autoscaleMin, r.cfg.autoscaleMax, r.cfg.extraLabels)
}

// autoscaleCycle proves the cluster-autoscaler scales the worker nodegroup UP
// (pending pods → add workers, past 2) and back DOWN (idle → remove workers, to
// the floor). It first patches the deployed autoscaler with short scale-down
// timers so the down phase completes in minutes (the reconciler re-deploys the
// autoscaler with stock timers on the next upgrade, so each rung re-patches).
func (r *runner) autoscaleCycle(ctx context.Context) error {
	r.log("=== autoscale: cluster-autoscaler up >2 then back to %d ===", r.cfg.autoscaleMin)
	if r.cfg.autoscaleMax <= 2 {
		return fmt.Errorf("autoscale-max=%d must be >2 to prove scale-up past 2", r.cfg.autoscaleMax)
	}
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	if err := r.waitAutoscalerReady(ctx, kc); err != nil {
		return err
	}
	if err := r.patchAutoscalerFastTiming(ctx, kc); err != nil {
		return err
	}

	base, err := r.countReadyWorkers(ctx, kc)
	if err != nil {
		return err
	}
	r.log("autoscale: baseline %d ready worker(s); driving UP to %d", base, r.cfg.autoscaleMax)

	// Scale UP: a balloon Deployment with hostname anti-affinity forces one pod
	// per node; more replicas than nodes ⇒ pending pods ⇒ autoscaler adds workers.
	if err := r.createBalloon(ctx, kc, r.cfg.autoscaleMax); err != nil {
		return err
	}
	upErr := r.waitCount(ctx, "scale-up", 25*time.Minute,
		func(c context.Context) (int, error) { return r.countReadyWorkers(c, kc) },
		func(n int) bool { return n > 2 && n >= r.cfg.autoscaleMax })
	if upErr != nil {
		r.deleteBalloon(ctx, kc)
		return upErr
	}
	r.log("autoscale: scaled UP to >2 workers ✅")

	// Scale DOWN: remove the load; idle workers are reclaimed. Count the worker
	// nodegroup's desired size (authoritative) rather than k8s nodes, which lag on
	// scaledown until the cloud-node-lifecycle controller deletes the Node object.
	r.deleteBalloon(ctx, kc)
	if err := r.waitCount(ctx, "scale-down", 25*time.Minute,
		func(c context.Context) (int, error) { return r.workerNGCount(c) },
		func(n int) bool { return n <= r.cfg.autoscaleMin }); err != nil {
		return err
	}
	r.log("autoscale: scaled DOWN to %d worker(s) ✅", r.cfg.autoscaleMin)

	// The autoscaler resizes via Magnum, so the cluster goes UPDATE_*; settle
	// before the next op (else the following upgrade hits a busy cluster).
	return r.ensureSettled(ctx)
}

// autoscalerDeployment resolves the deployed cluster-autoscaler Deployment by its
// expected name, falling back to any kube-system deployment whose name contains
// "autoscaler" (chart naming can vary across versions).
func (r *runner) autoscalerDeployment(ctx context.Context, kc *kubernetes.Clientset) (*appsv1.Deployment, error) {
	d, err := kc.AppsV1().Deployments(autoscalerNS).Get(ctx, autoscalerDeploy, metav1.GetOptions{})
	if err == nil {
		return d, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	list, lerr := kc.AppsV1().Deployments(autoscalerNS).List(ctx, metav1.ListOptions{})
	if lerr != nil {
		return nil, lerr
	}
	for i := range list.Items {
		if strings.Contains(list.Items[i].Name, "autoscaler") {
			return &list.Items[i], nil
		}
	}
	return nil, fmt.Errorf("cluster-autoscaler deployment not found in %s — was AUTO_SCALING_ENABLED set at create?", autoscalerNS)
}

func (r *runner) waitAutoscalerReady(ctx context.Context, kc *kubernetes.Clientset) error {
	deadline := time.Now().Add(8 * time.Minute)
	for {
		d, err := r.autoscalerDeployment(ctx, kc)
		if err == nil && d.Status.AvailableReplicas >= 1 {
			r.log("autoscale: %s Ready (%d replica)", d.Name, d.Status.AvailableReplicas)
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("cluster-autoscaler not Ready within timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// patchAutoscalerFastTiming rewrites the autoscaler's scale-down timing flags to
// short values so the scale-down phase completes in minutes instead of the ~10m
// chart defaults. The reconciler reverts this on its next Helm apply (each
// upgrade), which is why every rung re-patches.
func (r *runner) patchAutoscalerFastTiming(ctx context.Context, kc *kubernetes.Clientset) error {
	d, err := r.autoscalerDeployment(ctx, kc)
	if err != nil {
		return err
	}
	cs := d.Spec.Template.Spec.Containers
	if len(cs) == 0 {
		return fmt.Errorf("autoscaler deployment %s has no containers", d.Name)
	}
	ci := 0
	for i := range cs {
		if strings.Contains(cs[i].Image, "cluster-autoscaler") {
			ci = i
			break
		}
	}
	fast := map[string]string{
		"scale-down-delay-after-add":     "20s",
		"scale-down-delay-after-delete":  "10s",
		"scale-down-delay-after-failure": "20s",
		"scale-down-unneeded-time":       "20s",
		"scan-interval":                  "10s",
	}
	c := &cs[ci]
	// The autoscaler's flags live in Args (chart default) or Command; edit whichever holds them.
	if hasFlags(c.Args) {
		c.Args = setFlags(c.Args, fast)
	} else {
		c.Command = setFlags(c.Command, fast)
	}

	updated, err := kc.AppsV1().Deployments(autoscalerNS).Update(ctx, d, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("patch autoscaler timing: %w", err)
	}
	r.log("autoscale: patched %s with fast scale-down timers; waiting for rollout", updated.Name)

	gen := updated.Generation
	deadline := time.Now().Add(5 * time.Minute)
	for {
		cur, gerr := kc.AppsV1().Deployments(autoscalerNS).Get(ctx, updated.Name, metav1.GetOptions{})
		if gerr == nil && cur.Status.ObservedGeneration >= gen && cur.Status.UpdatedReplicas >= 1 && cur.Status.AvailableReplicas >= 1 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("autoscaler rollout after timing patch did not complete in time")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// hasFlags reports whether a command/args slice carries CLI flags ("--...").
func hasFlags(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "--") {
			return true
		}
	}
	return false
}

// setFlags drops any existing occurrences of the given flags from args and
// appends them with the desired values, preserving all other args in order.
func setFlags(args []string, flags map[string]string) []string {
	out := make([]string, 0, len(args)+len(flags))
	for _, a := range args {
		drop := false
		for k := range flags {
			if a == "--"+k || strings.HasPrefix(a, "--"+k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, a)
		}
	}
	for k, v := range flags {
		out = append(out, "--"+k+"="+v)
	}
	return out
}

// createBalloon deploys `replicas` pause pods with required hostname anti-affinity
// (one pod per node). With more replicas than current nodes the surplus pods stay
// Pending, which is what drives the autoscaler to add workers — flavor-independent
// (no reliance on per-node CPU/memory sizing).
func (r *runner) createBalloon(ctx context.Context, kc *kubernetes.Clientset, replicas int) error {
	reps := int32(replicas)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: balloonDeploy, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &reps,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": balloonApp}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": balloonApp}},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						// Worker-only: exclude control-plane nodes (which are often
						// schedulable here) so each replica forces a *worker* — and
						// one pod per node, so replicas>workers ⇒ pending ⇒ scale-up.
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{{
									MatchExpressions: workerOnlyNodeSelector(),
								}},
							},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": balloonApp}},
								TopologyKey:   "kubernetes.io/hostname",
							}},
						},
					},
					Containers: []corev1.Container{{
						Name:  "pause",
						Image: "registry.k8s.io/pause:3.9",
					}},
				},
			},
		},
	}
	if _, err := kc.AppsV1().Deployments("default").Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create balloon deployment: %w", err)
	}
	return nil
}

// workerOnlyNodeSelector requires a node to carry NEITHER control-plane label
// (both requirements in one term are AND-ed), pinning balloon pods to workers.
func workerOnlyNodeSelector() []corev1.NodeSelectorRequirement {
	reqs := make([]corev1.NodeSelectorRequirement, 0, len(controlPlaneLabels))
	for _, l := range controlPlaneLabels {
		reqs = append(reqs, corev1.NodeSelectorRequirement{
			Key:      l,
			Operator: corev1.NodeSelectorOpDoesNotExist,
		})
	}
	return reqs
}

func (r *runner) deleteBalloon(ctx context.Context, kc *kubernetes.Clientset) {
	_ = kc.AppsV1().Deployments("default").Delete(ctx, balloonDeploy, metav1.DeleteOptions{})
}

// countReadyWorkers returns the number of Ready worker nodes (no control-plane
// label) — the live count that proves added workers actually joined.
func (r *runner) countReadyWorkers(ctx context.Context, kc *kubernetes.Clientset) (int, error) {
	nodes, err := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if isControlPlane(node) {
			continue
		}
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				n++
				break
			}
		}
	}
	return n, nil
}

// workerNGCount returns the worker nodegroup's desired node count from Magnum —
// authoritative for scale-down (k8s Node objects linger after the VM is gone).
func (r *runner) workerNGCount(ctx context.Context) (int, error) {
	ng, err := r.resolveNodeGroup(ctx, "worker")
	if err != nil {
		return 0, err
	}
	return ng.NodeCount, nil
}

// waitCount polls get() until pred(count) holds or the timeout elapses, logging
// progress. Transient get() errors are logged and retried (the cluster is in
// UPDATE_* while the autoscaler resizes, so reads can blip).
func (r *runner) waitCount(ctx context.Context, phase string, timeout time.Duration, get func(context.Context) (int, error), pred func(int) bool) error {
	deadline := time.Now().Add(timeout)
	for {
		n, err := get(ctx)
		switch {
		case err != nil:
			r.log("autoscale: %s — count error (ignored): %v", phase, err)
		case pred(n):
			return nil
		default:
			r.log("autoscale: %s — count=%d (waiting)", phase, n)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("autoscale %s did not converge within %s", phase, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Second):
		}
	}
}

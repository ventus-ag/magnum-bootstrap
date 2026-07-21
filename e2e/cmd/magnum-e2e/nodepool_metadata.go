package main

import (
	"context"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/nodegroups"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The nodepool-metadata op exercises the per-nodegroup node_labels/node_taints
// pipeline end to end on the ACTIVE extra nodepool, through four PATCH stages:
//
//	add-single    — one label + one NoSchedule taint
//	add-multiple  — grow to three labels (including a node-role.kubernetes.io/*
//	                one, which only the API/node-manager path can apply) and
//	                two taints
//	delete-single — drop ONE label and ONE taint, the rest must survive
//	delete-all    — remove everything; the pool ends untainted
//
// After every stage the op waits until every nodepool Node object matches the
// expected present/absent sets (each PATCH is a params-only stack update the
// fork pushes to the nodegroup stack; the node reconciler then converges the
// Node). Scheduling is proven around the taint lifecycle: while tainted an
// untolerated pod pinned to the pool must be Unschedulable and a tolerated one
// must run; after delete-all the untolerated pod shape must run.
//
// The op is symmetric (everything it adds it deletes) so later verifyBundle
// runs can schedule their untolerated probe pod on the pool again.
const (
	npLabelTeamKey  = "e2e-team"
	npLabelTeamVal  = "magnum"
	npLabelEnvKey   = "e2e-env"
	npLabelEnvVal   = "ci"
	npLabelRoleKey  = "node-role.kubernetes.io/e2e-tier"
	npTaintMainKey  = "e2e-dedicated"
	npTaintMainVal  = "np"
	npTaintExtraKey = "e2e-phase"
	npTaintExtraVal = "test"
)

// npExpectedTaint is one taint expected on (or absent from) the pool's nodes.
type npExpectedTaint struct {
	key    string
	value  string
	effect corev1.TaintEffect
}

// npMetaStage is one PATCH of the nodegroup's node_labels/node_taints plus the
// node-level state that must converge afterwards.
type npMetaStage struct {
	name         string
	nodeLabels   string // node_labels Magnum label value ("" removes the key)
	nodeTaints   string // node_taints Magnum label value ("" removes the key)
	wantLabels   map[string]string
	absentLabels []string
	wantTaints   []npExpectedTaint
	absentTaints []string
}

var npMetaStages = []npMetaStage{
	{
		name:       "add-single",
		nodeLabels: npLabelTeamKey + "=" + npLabelTeamVal,
		nodeTaints: npTaintMainKey + "=" + npTaintMainVal + ":NoSchedule",
		wantLabels: map[string]string{npLabelTeamKey: npLabelTeamVal},
		wantTaints: []npExpectedTaint{{npTaintMainKey, npTaintMainVal, corev1.TaintEffectNoSchedule}},
	},
	{
		name: "add-multiple",
		nodeLabels: npLabelTeamKey + "=" + npLabelTeamVal + ";" +
			npLabelEnvKey + "=" + npLabelEnvVal + ";" +
			npLabelRoleKey + "=",
		// The second taint is PreferNoSchedule so the tolerated workloads from
		// the previous stage keep running regardless.
		nodeTaints: npTaintMainKey + "=" + npTaintMainVal + ":NoSchedule;" +
			npTaintExtraKey + "=" + npTaintExtraVal + ":PreferNoSchedule",
		wantLabels: map[string]string{
			npLabelTeamKey: npLabelTeamVal,
			npLabelEnvKey:  npLabelEnvVal,
			npLabelRoleKey: "",
		},
		wantTaints: []npExpectedTaint{
			{npTaintMainKey, npTaintMainVal, corev1.TaintEffectNoSchedule},
			{npTaintExtraKey, npTaintExtraVal, corev1.TaintEffectPreferNoSchedule},
		},
	},
	{
		name:       "delete-single",
		nodeLabels: npLabelTeamKey + "=" + npLabelTeamVal + ";" + npLabelRoleKey + "=",
		nodeTaints: npTaintMainKey + "=" + npTaintMainVal + ":NoSchedule",
		wantLabels: map[string]string{
			npLabelTeamKey: npLabelTeamVal,
			npLabelRoleKey: "",
		},
		absentLabels: []string{npLabelEnvKey},
		wantTaints:   []npExpectedTaint{{npTaintMainKey, npTaintMainVal, corev1.TaintEffectNoSchedule}},
		absentTaints: []string{npTaintExtraKey},
	},
	{
		name:         "delete-all",
		nodeLabels:   "",
		nodeTaints:   "",
		absentLabels: []string{npLabelTeamKey, npLabelEnvKey, npLabelRoleKey},
		absentTaints: []string{npTaintMainKey, npTaintExtraKey},
	},
}

// nodepoolMetadataCycle is the "nodepool-metadata" op.
func (r *runner) nodepoolMetadataCycle(ctx context.Context) error {
	poolName := r.nodepoolName()
	if !r.nodepoolActive {
		return fmt.Errorf("nodepool-metadata op requires an active nodepool — put add-nodepool before it in the op chain")
	}
	supported, err := r.nodepoolMetadataSupported(ctx)
	if err != nil {
		return err
	}
	if !supported {
		return fmt.Errorf("nodepool-metadata: %s", nodepoolMetadataUnsupportedMsg)
	}
	for _, stage := range npMetaStages {
		// No verifyBundle per stage: its nodepool probe pod is untolerated and
		// would fail while the pool is tainted. Each stage runs its own
		// targeted verification; one full bundle runs at the end.
		if err := r.runMutationNoBundle(ctx, "nodepool-metadata "+stage.name, func() error {
			return r.patchNodepoolMetadata(ctx, stage.nodeLabels, stage.nodeTaints)
		}); err != nil {
			return err
		}
		if err := r.waitNodepoolNodeMetadata(ctx, stage); err != nil {
			return err
		}
		switch stage.name {
		case "add-single":
			// Taint just appeared: untolerated pod must be rejected, tolerated
			// one must run.
			if err := r.verifyTaintBlocksScheduling(ctx); err != nil {
				return err
			}
			if err := r.verifyTolerationSchedules(ctx); err != nil {
				return err
			}
		case "delete-all":
			// Taint removal must converge back to a normally-schedulable pool.
			if err := r.verifyUntoleratedSchedules(ctx); err != nil {
				return err
			}
		}
	}
	r.log("nodepool %q metadata add/delete cycle (single+multiple) OK ✅", poolName)
	// The pool is untainted again — the standard bundle (including the
	// untolerated nodepool schedulability probe) must pass.
	return r.verifyBundle(ctx, "nodepool-metadata", false)
}

// nodepoolMetadataSupported reports whether the DEPLOYED Magnum renders the
// node_labels/node_taints heat params into the nodepool's stack. A pre-feature
// magnum_victoria accepts the labels on the nodegroup (arbitrary labels always
// pass) but silently drops them before heat-params — the node then never sees
// the metadata and the convergence wait can only time out. Probing the stack
// parameters turns that 25-minute red herring into an immediate, actionable
// failure (these ops are required gates and never skip).
func (r *runner) nodepoolMetadataSupported(ctx context.Context) (bool, error) {
	ng, err := r.resolveNodeGroupByName(ctx, r.nodepoolName())
	if err != nil {
		return false, err
	}
	orch, err := r.orchClient()
	if err != nil {
		return false, err
	}
	var resp struct {
		Stack struct {
			Parameters map[string]any `json:"parameters"`
		} `json:"stack"`
	}
	if _, err := orch.Get(ctx, orch.ServiceURL("stacks", ng.StackID), &resp, &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
		return false, fmt.Errorf("get nodepool stack %s: %w", ng.StackID, err)
	}
	_, ok := resp.Stack.Parameters["node_labels"]
	return ok, nil
}

const nodepoolMetadataUnsupportedMsg = "deployed Magnum does not render node_labels/node_taints heat params " +
	"(pre-feature magnum_victoria on this cloud) — metadata is stored on the nodegroup but never reaches the nodes; " +
	"this cloud must run the magnum_victoria fork with nodepool metadata support"

// nodepoolMetadataSmoke is the "nodepool-metadata-smoke" op: a compact
// create → verify → remove → verify → delete lifecycle for the smoke scenario.
// Unlike the full nodepool-metadata op (which patches an existing pool through
// four stages), this CREATES a fresh nodepool with a label + a NoSchedule
// taint baked in at creation (exercising the kubelet registration path), then
// removes them via a single PATCH, then deletes the pool. Self-contained — it
// owns the pool's whole lifetime, so it needs no add-nodepool/del-nodepool
// bracketing in the op chain.
func (r *runner) nodepoolMetadataSmoke(ctx context.Context) error {
	nodeLabels := npLabelTeamKey + "=" + npLabelTeamVal
	nodeTaints := npTaintMainKey + "=" + npTaintMainVal + ":NoSchedule"

	// 1. Create the pool with the label + taint baked in.
	prevActive := r.nodepoolActive
	r.nodepoolActive = true
	if err := r.runMutationNoBundle(ctx, "nodepool-metadata-smoke create (label+taint)", func() error {
		return r.triggerNodepoolCreateMetadata(ctx, 1, nodeLabels, nodeTaints)
	}); err != nil {
		r.nodepoolActive = prevActive
		return err
	}

	// Fail FAST (seconds, with the real reason) when the cloud's Magnum
	// predates the feature — otherwise the convergence wait below can only
	// burn its full timeout on a misleading "label missing". This op is a
	// required gate: it must never silently skip.
	supported, err := r.nodepoolMetadataSupported(ctx)
	if err != nil {
		return err
	}
	if !supported {
		return fmt.Errorf("nodepool-metadata-smoke: %s", nodepoolMetadataUnsupportedMsg)
	}

	present := npMetaStage{
		name:       "smoke-present",
		wantLabels: map[string]string{npLabelTeamKey: npLabelTeamVal},
		wantTaints: []npExpectedTaint{{key: npTaintMainKey, value: npTaintMainVal, effect: corev1.TaintEffectNoSchedule}},
	}
	if err := r.waitNodepoolNodeMetadata(ctx, present); err != nil {
		return err
	}
	// The taint was set at registration — prove it: untolerated pod rejected,
	// tolerated pod runs.
	if err := r.verifyTaintBlocksScheduling(ctx); err != nil {
		return err
	}
	if err := r.verifyTolerationSchedules(ctx); err != nil {
		return err
	}

	// 2. Remove the label + taint via a single PATCH; verify the node converges.
	if err := r.runMutationNoBundle(ctx, "nodepool-metadata-smoke remove (label+taint)", func() error {
		return r.patchNodepoolMetadata(ctx, "", "")
	}); err != nil {
		return err
	}
	absent := npMetaStage{
		name:         "smoke-absent",
		absentLabels: []string{npLabelTeamKey},
		absentTaints: []string{npTaintMainKey},
	}
	if err := r.waitNodepoolNodeMetadata(ctx, absent); err != nil {
		return err
	}
	// Taint gone → the untolerated pod shape now schedules.
	if err := r.verifyUntoleratedSchedules(ctx); err != nil {
		return err
	}

	// 3. Delete the pool (full bundle after: pool gone, cluster back to base).
	r.nodepoolActive = false
	if err := r.runMutation(ctx, "nodepool-metadata-smoke delete", true, func() error {
		return r.triggerNodepoolDelete(ctx)
	}); err != nil {
		r.nodepoolActive = true
		return err
	}
	r.log("nodepool metadata smoke (create+label+taint → remove → delete) OK ✅")
	return nil
}

// patchNodepoolMetadata sets/removes the nodepool's node_labels/node_taints
// Magnum labels (empty string removes the key) via a raw JSON-patch —
// gophercloud's nodegroups.Update builder only whitelists the min/max node
// count paths. Ops are PER-KEY ("/labels/node_labels"): Magnum's
// JsonPatchType only accepts string/int values, so a whole-map
// `replace /labels {dict}` is rejected with a wsme 400. "add" upserts per RFC
// 6902; "remove" is only emitted when the key exists (strict jsonpatch errors
// on removing a missing key).
func (r *runner) patchNodepoolMetadata(ctx context.Context, nodeLabels, nodeTaints string) error {
	resolved, err := r.resolveNodeGroupByName(ctx, r.nodepoolName())
	if err != nil {
		return err
	}
	// The nodegroup LIST view is a summary WITHOUT labels — diffing against it
	// makes every key look absent. Fetch the detail view for the real labels.
	ng, err := nodegroups.Get(ctx, r.magnum, r.cfg.clusterName, resolved.UUID).Extract()
	if err != nil {
		return fmt.Errorf("get nodegroup %q detail: %w", resolved.Name, err)
	}
	var ops []map[string]any
	setOrRemove := func(key, value string) {
		_, exists := ng.Labels[key]
		switch {
		case value != "":
			ops = append(ops, map[string]any{"op": "add", "path": "/labels/" + key, "value": value})
		case exists:
			ops = append(ops, map[string]any{"op": "remove", "path": "/labels/" + key})
		}
	}
	setOrRemove("node_labels", nodeLabels)
	setOrRemove("node_taints", nodeTaints)
	if len(ops) == 0 {
		// Nothing would change → Magnum would never start an UPDATE and the
		// caller's transition wait could only hang. Fail loud instead.
		return fmt.Errorf("patch nodegroup %q: no metadata changes to apply (node_labels=%q node_taints=%q already absent)", ng.Name, nodeLabels, nodeTaints)
	}
	url := r.magnum.ServiceURL("clusters", r.cfg.clusterName, "nodegroups", ng.UUID)
	if _, err := r.magnum.Patch(ctx, url, ops, nil, &gophercloud.RequestOpts{OkCodes: []int{200, 202}}); err != nil {
		return fmt.Errorf("patch nodegroup %q labels: %w", ng.Name, err)
	}
	r.log("nodepool %q labels patched (%d op(s)): node_labels=%q node_taints=%q", ng.Name, len(ops), nodeLabels, nodeTaints)
	return nil
}

// waitNodepoolNodeMetadata polls the nodepool's Node objects until every node
// matches the stage's expected present/absent label and taint sets. The
// reconciler applies changes after the Heat deployment re-fires, so this can
// take several minutes past UPDATE_COMPLETE.
func (r *runner) waitNodepoolNodeMetadata(ctx context.Context, stage npMetaStage) error {
	poolName := r.nodepoolName()
	r.log("verify: nodepool %q node metadata for stage %q", poolName, stage.name)
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(25 * time.Minute)
	var lastWhy string
	for {
		nodes, lerr := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: "magnum.openstack.org/nodegroup=" + poolName,
		})
		switch {
		case lerr != nil:
			lastWhy = fmt.Sprintf("list nodes: %v", lerr)
		case len(nodes.Items) == 0:
			lastWhy = "no nodes carry the nodegroup label"
		default:
			lastWhy = ""
			for _, node := range nodes.Items {
				if why := nodeMetadataMismatch(node, stage); why != "" {
					lastWhy = fmt.Sprintf("node %s: %s", node.Name, why)
					break
				}
			}
			if lastWhy == "" {
				r.log("nodepool %q: %d node(s) match stage %q ✅", poolName, len(nodes.Items), stage.name)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nodepool %q metadata did not converge for stage %q: %s", poolName, stage.name, lastWhy)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

// nodeMetadataMismatch returns "" when the node matches the stage's expected
// state, else a human-readable reason.
func nodeMetadataMismatch(node corev1.Node, stage npMetaStage) string {
	// Workers carry NO node-role.kubernetes.io/* label by default (empty ROLES,
	// matching GKE/kubeadm); the canonical worker identity is
	// magnum.openstack.org/role=worker, set by the kubelet at registration.
	if node.Labels["magnum.openstack.org/role"] != "worker" {
		return "label magnum.openstack.org/role != worker"
	}
	for key, want := range stage.wantLabels {
		got, ok := node.Labels[key]
		if !ok {
			return fmt.Sprintf("label %s missing", key)
		}
		if got != want {
			return fmt.Sprintf("label %s=%q != %q", key, got, want)
		}
	}
	for _, key := range stage.absentLabels {
		if _, ok := node.Labels[key]; ok {
			return fmt.Sprintf("label %s still present", key)
		}
	}
	taintsByKey := map[string]corev1.Taint{}
	for _, taint := range node.Spec.Taints {
		taintsByKey[taint.Key] = taint
	}
	for _, want := range stage.wantTaints {
		got, ok := taintsByKey[want.key]
		if !ok {
			return fmt.Sprintf("taint %s missing", want.key)
		}
		if got.Value != want.value || got.Effect != want.effect {
			return fmt.Sprintf("taint %s is %s:%s, want %s:%s", want.key, got.Value, got.Effect, want.value, want.effect)
		}
	}
	for _, key := range stage.absentTaints {
		if _, ok := taintsByKey[key]; ok {
			return fmt.Sprintf("taint %s still present", key)
		}
	}
	return ""
}

func (r *runner) nodepoolProbePod(name string, tolerate bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeSelector:  map[string]string{"magnum.openstack.org/nodegroup": r.nodepoolName()},
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "pause",
				Image: "registry.k8s.io/pause:3.9",
			}},
		},
	}
	if tolerate {
		pod.Spec.Tolerations = []corev1.Toleration{{
			Key:      npTaintMainKey,
			Operator: corev1.TolerationOpEqual,
			Value:    npTaintMainVal,
			Effect:   corev1.TaintEffectNoSchedule,
		}}
	}
	return pod
}

// verifyTaintBlocksScheduling proves the taint is enforced: an untolerated pod
// pinned to the pool must be reported Unschedulable by the scheduler (and must
// not start running).
func (r *runner) verifyTaintBlocksScheduling(ctx context.Context) error {
	r.log("verify: taint blocks untolerated pod on nodepool %q", r.nodepoolName())
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	const podName = "e2e-np-untolerated"
	pod := r.nodepoolProbePod(podName, false)
	_ = kc.CoreV1().Pods("default").Delete(ctx, podName, metav1.DeleteOptions{})
	if _, err := kc.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create untolerated probe pod: %w", err)
	}
	defer func() {
		_ = kc.CoreV1().Pods("default").Delete(ctx, podName, metav1.DeleteOptions{})
	}()

	deadline := time.Now().Add(3 * time.Minute)
	for {
		p, gerr := kc.CoreV1().Pods("default").Get(ctx, podName, metav1.GetOptions{})
		if gerr == nil {
			if p.Status.Phase == corev1.PodRunning {
				return fmt.Errorf("untolerated pod reached Running on %s — taint not enforced", p.Spec.NodeName)
			}
			for _, cond := range p.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == corev1.PodReasonUnschedulable {
					r.log("untolerated pod correctly Unschedulable (%s) ✅", cond.Message)
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("untolerated pod neither Unschedulable nor Running after 3m — cannot prove taint")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// verifyTolerationSchedules proves the tainted pool still accepts tolerated
// workloads (the labels routed it, the toleration admitted it).
func (r *runner) verifyTolerationSchedules(ctx context.Context) error {
	return r.waitProbePodRunning(ctx, "e2e-np-tolerated", true)
}

// verifyUntoleratedSchedules proves the taint REMOVAL converged: the same pod
// shape that was Unschedulable earlier must now run.
func (r *runner) verifyUntoleratedSchedules(ctx context.Context) error {
	return r.waitProbePodRunning(ctx, "e2e-np-untainted", false)
}

func (r *runner) waitProbePodRunning(ctx context.Context, podName string, tolerate bool) error {
	r.log("verify: probe pod %s (tolerate=%v) reaches Running on nodepool %q", podName, tolerate, r.nodepoolName())
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	pod := r.nodepoolProbePod(podName, tolerate)
	_ = kc.CoreV1().Pods("default").Delete(ctx, podName, metav1.DeleteOptions{})
	if _, err := kc.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create probe pod %s: %w", podName, err)
	}
	defer func() {
		_ = kc.CoreV1().Pods("default").Delete(ctx, podName, metav1.DeleteOptions{})
	}()

	deadline := time.Now().Add(5 * time.Minute)
	for {
		p, gerr := kc.CoreV1().Pods("default").Get(ctx, podName, metav1.GetOptions{})
		if gerr == nil && p.Status.Phase == corev1.PodRunning {
			r.log("probe pod %s Running on node %s ✅", podName, p.Spec.NodeName)
			return nil
		}
		if time.Now().After(deadline) {
			phase := "unknown"
			if gerr == nil {
				phase = string(p.Status.Phase)
			}
			return fmt.Errorf("probe pod %s did not reach Running (phase=%s)", podName, phase)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

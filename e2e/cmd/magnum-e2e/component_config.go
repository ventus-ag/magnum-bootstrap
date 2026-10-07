package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/clusters"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Label-driven component configuration. The probe flags are harmless and
// observable through the API: DefaultTolerationSeconds admission stamps every
// new pod with not-ready/unreachable tolerations of this many seconds
// (default 300). The audit policy travels as a kube_file_<name> label, so the
// probe also proves arbitrary files and the args that reference them land
// together.
const (
	probeTolerationSeconds   = 301
	defaultTolerationSeconds = 300
	e2eKubeFile              = "e2e_audit_policy"
	kubeFilesDir             = "/etc/kubernetes/files" // kubecommon.KubeFilesDir
	e2eAuditPolicy           = "apiVersion: audit.k8s.io/v1\nkind: Policy\nrules:\n- level: None\n"
	schedulerScoringLabel    = "kube_scheduler_scoring_strategy"
)

func componentArgsLabels() map[string]string {
	return map[string]string{
		"kubeapi_options": fmt.Sprintf("--default-not-ready-toleration-seconds=%d --default-unreachable-toleration-seconds=%d --audit-policy-file=%s/%s --audit-log-path=-",
			probeTolerationSeconds, probeTolerationSeconds, kubeFilesDir, e2eKubeFile),
		"kube_file_" + e2eKubeFile: e2eAuditPolicy,
	}
}

// labelPatchOpts builds one JSON-patch op per label (add creates or replaces).
// Magnum rejects a whole-/labels value, so keys are patched individually.
func labelPatchOpts(set map[string]string, unset []string) []clusters.UpdateOpts {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	opts := make([]clusters.UpdateOpts, 0, len(set)+len(unset))
	for _, k := range keys {
		opts = append(opts, clusters.UpdateOpts{Op: clusters.AddOp, Path: "/labels/" + k, Value: set[k]})
	}
	for _, k := range unset {
		opts = append(opts, clusters.UpdateOpts{Op: clusters.RemoveOp, Path: "/labels/" + k})
	}
	return opts
}

// patchClusterLabels applies set/unset in ONE PATCH, so the reconciler converges
// every change in a single reconfigure.
func (r *runner) patchClusterLabels(ctx context.Context, set map[string]string, unset []string) error {
	if _, err := clusters.Update(ctx, r.magnum, r.cfg.clusterName, labelPatchOpts(set, unset)).Extract(); err != nil {
		return fmt.Errorf("patch cluster labels set=%v unset=%v: %w", set, unset, err)
	}
	return nil
}

func (r *runner) setComponentArgs(ctx context.Context) error {
	prev := r.componentArgs
	r.componentArgs = true
	if err := r.runMutationNoBundle(ctx, "set-component-args", func() error {
		return r.patchClusterLabels(ctx, componentArgsLabels(), nil)
	}); err != nil {
		r.componentArgs = prev
		return err
	}
	// From here every verify bundle re-asserts the args, so a later
	// resize/upgrade/rotation that reverts them fails at that op.
	return r.verifyBundle(ctx, "set-component-args", false)
}

func (r *runner) clearComponentArgs(ctx context.Context) error {
	r.componentArgs = false
	if err := r.runMutation(ctx, "clear-component-args", false, func() error {
		return r.patchClusterLabels(ctx, nil, []string{"kubeapi_options", "kube_file_" + e2eKubeFile})
	}); err != nil {
		return err
	}
	return r.verifyComponentArgs(ctx, false)
}

// verifyComponentArgs asserts every apiserver runs (or no longer runs) the
// label-set args, and that the shipped file is (or is no longer) on the masters.
func (r *runner) verifyComponentArgs(ctx context.Context, applied bool) error {
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	want := int64(defaultTolerationSeconds)
	if applied {
		want = probeTolerationSeconds
	}
	masters := 1
	if ng, err := r.resolveNodeGroup(ctx, "master"); err == nil && ng.NodeCount > 1 {
		masters = ng.NodeCount
	}
	// The API is load-balanced across masters; enough probes hit every one.
	for i := 0; i < 3*masters+2; i++ {
		got, err := dryRunTolerationSeconds(ctx, kc)
		if err != nil {
			return fmt.Errorf("component-args probe: %w", err)
		}
		if got != want {
			state := "were reverted on (or never reached)"
			if !applied {
				state = "are still active on"
			}
			return fmt.Errorf("component-args: pod default tolerationSeconds=%d, want %d — the label-set kubeapi_options %s at least one apiserver", got, want, state)
		}
	}
	return r.verifyKubeFileOnMasters(ctx, applied)
}

func dryRunTolerationSeconds(ctx context.Context, kc kubernetes.Interface) (int64, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-toleration-probe-", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "probe", Image: "registry.k8s.io/pause:3.9"}}},
	}
	created, err := kc.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		return 0, fmt.Errorf("dry-run pod create: %w", err)
	}
	return notReadyTolerationSeconds(created.Spec.Tolerations)
}

func notReadyTolerationSeconds(tolerations []corev1.Toleration) (int64, error) {
	for _, t := range tolerations {
		if t.Key == "node.kubernetes.io/not-ready" && t.TolerationSeconds != nil {
			return *t.TolerationSeconds, nil
		}
	}
	return 0, errors.New("no not-ready toleration injected (DefaultTolerationSeconds admission disabled?)")
}

// verifyKubeFileOnMasters checks the shipped file over SSH. Unreachable SSH is
// logged, not fatal: the API probe above already proved the apiserver read it.
func (r *runner) verifyKubeFileOnMasters(ctx context.Context, present bool) error {
	masters, err := r.nodesByRole(ctx, "master")
	if err != nil {
		r.log("component-args: SKIP file check, cannot list masters: %v", err)
		return nil
	}
	path := kubeFilesDir + "/" + e2eKubeFile
	for _, m := range masters {
		out, err := r.nodeExec(ctx, m, "sudo cat "+path)
		var exitErr *ssh.ExitError
		switch {
		case err != nil && !errors.As(err, &exitErr):
			r.log("component-args: SKIP file check on %s, SSH unavailable: %v", m.name, err)
		case present && (err != nil || out != e2eAuditPolicy):
			return fmt.Errorf("component-args: %s on %s = %q (err=%v), want the kube_file_%s label content", path, m.name, out, err, e2eKubeFile)
		case !present && err == nil:
			return fmt.Errorf("component-args: %s still on %s after its label was removed", path, m.name)
		}
	}
	return nil
}

// patchNodeCount drives the cluster-update node_count path (update_cluster →
// _update_stack), a parent-stack update distinct from the resize action.
func (r *runner) patchNodeCount(ctx context.Context, n int) error {
	opts := []clusters.UpdateOpts{{Op: clusters.ReplaceOp, Path: "/node_count", Value: n}}
	if _, err := clusters.Update(ctx, r.magnum, r.cfg.clusterName, opts).Extract(); err != nil {
		return fmt.Errorf("patch node_count=%d: %w", n, err)
	}
	return nil
}

// schedulerScoringCycle proves kube_scheduler_scoring_strategy=MostAllocated
// bin-packs: identical pods land on ONE worker, where the default spreads them.
func (r *runner) schedulerScoringCycle(ctx context.Context) error {
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	ng, err := r.resolveNodeGroup(ctx, "worker")
	if err != nil {
		return err
	}
	nodes, err := readyNodegroupNodes(ctx, kc, ng.Name)
	if err != nil {
		return err
	}
	if len(nodes) < 2 {
		return fmt.Errorf("scheduler-scoring needs >= 2 Ready workers in nodegroup %s, have %d", ng.Name, len(nodes))
	}
	if spread, err := r.packingProbe(ctx, kc, ng.Name, nodes); err == nil {
		r.log("scheduler-scoring: control (default scoring) placed probe pods on %d node(s)", spread)
	}

	if err := r.runMutation(ctx, "scheduler-scoring=MostAllocated", false, func() error {
		return r.patchClusterLabels(ctx, map[string]string{schedulerScoringLabel: "MostAllocated"}, nil)
	}); err != nil {
		return err
	}
	if err := waitSchedulerLeader(ctx, kc); err != nil {
		return err
	}
	spread, err := r.packingProbe(ctx, kc, ng.Name, nodes)
	if err != nil {
		return err
	}
	if spread != 1 {
		return fmt.Errorf("scheduler-scoring: MostAllocated placed probe pods on %d nodes, want 1 (bin-packing)", spread)
	}
	r.log("scheduler-scoring: MostAllocated packed every probe pod onto one node")

	if err := r.runMutation(ctx, "scheduler-scoring reset", false, func() error {
		return r.patchClusterLabels(ctx, nil, []string{schedulerScoringLabel})
	}); err != nil {
		return err
	}
	return waitSchedulerLeader(ctx, kc)
}

func readyNodegroupNodes(ctx context.Context, kc kubernetes.Interface, ngName string) ([]corev1.Node, error) {
	list, err := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "magnum.openstack.org/nodegroup=" + ngName})
	if err != nil {
		return nil, fmt.Errorf("list nodes of nodegroup %s: %w", ngName, err)
	}
	var ready []corev1.Node
	for _, n := range list.Items {
		if isControlPlane(&n) || n.Spec.Unschedulable {
			continue
		}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = append(ready, n)
			}
		}
	}
	return ready, nil
}

// probeCPURequest sizes each probe pod at 10% of the smallest node's
// allocatable CPU, so four of them always fit on one node.
func probeCPURequest(nodes []corev1.Node) resource.Quantity {
	minMilli := int64(0)
	for _, n := range nodes {
		cpu := n.Status.Allocatable.Cpu().MilliValue()
		if minMilli == 0 || cpu < minMilli {
			minMilli = cpu
		}
	}
	return *resource.NewMilliQuantity(max(minMilli/10, 100), resource.DecimalSI)
}

const packingProbePods = 4

// packingProbe schedules identical pods one at a time and returns how many
// distinct nodes they landed on. Bare pods: no owner, so no default spreading.
func (r *runner) packingProbe(ctx context.Context, kc kubernetes.Interface, ngName string, nodes []corev1.Node) (int, error) {
	cpu := probeCPURequest(nodes)
	var names []string
	defer func() {
		zero := int64(0)
		for _, name := range names {
			_ = kc.CoreV1().Pods("default").Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		}
	}()
	placed := map[string]bool{}
	for i := range packingProbePods {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-pack-", Namespace: "default", Labels: map[string]string{"app": "e2e-pack"}},
			Spec: corev1.PodSpec{
				NodeSelector: map[string]string{"magnum.openstack.org/nodegroup": ngName},
				Containers: []corev1.Container{{
					Name: "pause", Image: "registry.k8s.io/pause:3.9",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: cpu}},
				}},
			},
		}
		created, err := kc.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})
		if err != nil {
			return 0, fmt.Errorf("create probe pod %d: %w", i, err)
		}
		names = append(names, created.Name)
		node, err := waitScheduled(ctx, kc, created.Name)
		if err != nil {
			return 0, err
		}
		placed[node] = true
	}
	return len(placed), nil
}

func waitScheduled(ctx context.Context, kc kubernetes.Interface, name string) (string, error) {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		p, err := kc.CoreV1().Pods("default").Get(ctx, name, metav1.GetOptions{})
		if err == nil && p.Spec.NodeName != "" {
			return p.Spec.NodeName, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("probe pod %s not scheduled within 3m (scheduler down?)", name)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// waitSchedulerLeader waits for a kube-scheduler leader lease renewed in the
// last minute: a scheduler that fails to load its --config never renews it.
func waitSchedulerLeader(ctx context.Context, kc kubernetes.Interface) error {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		lease, err := kc.CoordinationV1().Leases("kube-system").Get(ctx, "kube-scheduler", metav1.GetOptions{})
		if err == nil && leaseFresh(lease.Spec.HolderIdentity, lease.Spec.RenewTime, time.Now()) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kube-scheduler holds no fresh leader lease within 5m (err=%v)", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func leaseFresh(holder *string, renew *metav1.MicroTime, now time.Time) bool {
	return holder != nil && *holder != "" && renew != nil && now.Sub(renew.Time) < time.Minute
}

// settingSwitch is a UI settings switch (a cluster label) and how its addon is
// observed in the cluster.
type settingSwitch struct {
	label   string
	present func(ctx context.Context, kc kubernetes.Interface) (bool, error)
}

var (
	dashboardSwitch = settingSwitch{label: "kube_dashboard_enabled", present: func(ctx context.Context, kc kubernetes.Interface) (bool, error) {
		return deploymentsAvailable(ctx, kc, "kubernetes-dashboard", "")
	}}
	autoHealingSwitch = settingSwitch{label: "auto_healing_enabled", present: func(ctx context.Context, kc kubernetes.Interface) (bool, error) {
		return daemonSetReady(ctx, kc, "kube-system", "magnum-auto-healer")
	}}
	cinderSwitch = settingSwitch{label: "cinder_csi_enabled", present: func(ctx context.Context, kc kubernetes.Interface) (bool, error) {
		return deploymentsAvailable(ctx, kc, "kube-system", "app=openstack-cinder-csi,component=controllerplugin")
	}}
	cloudProviderSwitch = settingSwitch{label: "cloud_provider_enabled", present: func(ctx context.Context, kc kubernetes.Interface) (bool, error) {
		return daemonSetReady(ctx, kc, "kube-system", "openstack-cloud-controller-manager")
	}}
)

func deploymentsAvailable(ctx context.Context, kc kubernetes.Interface, ns, selector string) (bool, error) {
	list, err := kc.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return false, err
	}
	for _, d := range list.Items {
		if d.Status.AvailableReplicas > 0 {
			return true, nil
		}
	}
	return false, nil
}

func daemonSetReady(ctx context.Context, kc kubernetes.Interface, ns, name string) (bool, error) {
	ds, err := kc.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ds.Status.NumberReady > 0, nil
}

// flipSwitches sets every switch to on in one PATCH and waits until each addon
// reaches want (which may differ from on when a guard keeps it installed).
func (r *runner) flipSwitches(ctx context.Context, step string, on bool, want map[string]bool, switches ...settingSwitch) error {
	set := map[string]string{}
	for _, s := range switches {
		set[s.label] = fmt.Sprint(on)
	}
	if err := r.runMutation(ctx, step, false, func() error { return r.patchClusterLabels(ctx, set, nil) }); err != nil {
		return err
	}
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	for _, s := range switches {
		expect, ok := want[s.label]
		if !ok {
			expect = on
		}
		if err := waitFor(ctx, 8*time.Minute, fmt.Sprintf("%s=%v: addon present=%v", s.label, on, expect), func() (bool, error) {
			got, err := s.present(ctx, kc)
			return err == nil && got == expect, nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func waitFor(ctx context.Context, timeout time.Duration, what string, cond func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := cond()
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// toggleSettingsCycle walks the UI settings switches through real transitions
// and the safety guards: Cinder CSI and the cloud provider stay installed while
// volumes / LoadBalancers depend on them, and Cinder's StorageClasses survive
// every transition.
func (r *runner) toggleSettingsCycle(ctx context.Context) error {
	if err := r.flipSwitches(ctx, "settings on (dashboard, auto-healing)", true, nil, dashboardSwitch, autoHealingSwitch); err != nil {
		return err
	}
	if err := r.flipSwitches(ctx, "settings off (dashboard, auto-healing)", false, nil, dashboardSwitch, autoHealingSwitch); err != nil {
		return err
	}
	if err := r.cinderSwitchCycle(ctx); err != nil {
		return err
	}
	return r.cloudProviderGuardCycle(ctx)
}

const guardPVC = "e2e-cinder-guard"

func (r *runner) cinderSwitchCycle(ctx context.Context) error {
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	if err := createBoundPVC(ctx, kc, guardPVC); err != nil {
		return err
	}
	// In use: switching off must keep the driver (and the classes).
	if err := r.flipSwitches(ctx, "cinder off while a volume is in use", false, map[string]bool{cinderSwitch.label: true}, cinderSwitch); err != nil {
		return err
	}
	if err := r.assertCinderStorageClasses(ctx, kc); err != nil {
		return err
	}
	if err := r.flipSwitches(ctx, "cinder on", true, nil, cinderSwitch); err != nil {
		return err
	}
	if err := deletePVCAndWait(ctx, kc, guardPVC); err != nil {
		return err
	}
	// Unused: the driver goes, the StorageClasses stay (helm resource-policy keep).
	if err := r.flipSwitches(ctx, "cinder off", false, nil, cinderSwitch); err != nil {
		return err
	}
	if err := r.assertCinderStorageClasses(ctx, kc); err != nil {
		return err
	}
	if err := r.flipSwitches(ctx, "cinder on again", true, nil, cinderSwitch); err != nil {
		return err
	}
	return r.assertCinderStorageClasses(ctx, kc)
}

func (r *runner) assertCinderStorageClasses(ctx context.Context, kc kubernetes.Interface) error {
	list, err := kc.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	found, defaults := map[string]bool{}, 0
	for _, sc := range list.Items {
		found[sc.Name] = true
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			defaults++
		}
	}
	for _, name := range []string{"csi-cinder-sc-delete", "csi-cinder-sc-retain"} {
		if !found[name] {
			return fmt.Errorf("StorageClass %s is gone — a Cinder CSI switch transition deleted it", name)
		}
	}
	if defaults > 1 {
		return fmt.Errorf("%d default StorageClasses, want at most 1", defaults)
	}
	return nil
}

func createBoundPVC(ctx context.Context, kc kubernetes.Interface, name string) error {
	sc := "csi-cinder-sc-delete"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	if _, err := kc.CoreV1().PersistentVolumeClaims("default").Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create PVC %s: %w", name, err)
	}
	return waitFor(ctx, 5*time.Minute, "PVC "+name+" Bound", func() (bool, error) {
		p, err := kc.CoreV1().PersistentVolumeClaims("default").Get(ctx, name, metav1.GetOptions{})
		return err == nil && p.Status.Phase == corev1.ClaimBound, nil
	})
}

func deletePVCAndWait(ctx context.Context, kc kubernetes.Interface, name string) error {
	p, err := kc.CoreV1().PersistentVolumeClaims("default").Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	pv := p.Spec.VolumeName
	if err := kc.CoreV1().PersistentVolumeClaims("default").Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return waitFor(ctx, 5*time.Minute, "PV "+pv+" released and deleted", func() (bool, error) {
		_, err := kc.CoreV1().PersistentVolumes().Get(ctx, pv, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
}

const guardService = "e2e-occm-guard"

func (r *runner) cloudProviderGuardCycle(ctx context.Context) error {
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: guardService, Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": guardService},
			Ports:    []corev1.ServicePort{{Port: 80}},
		},
	}
	if _, err := kc.CoreV1().Services("default").Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create LoadBalancer service: %w", err)
	}
	defer func() {
		_ = kc.CoreV1().Services("default").Delete(context.WithoutCancel(ctx), guardService, metav1.DeleteOptions{})
	}()
	if err := r.flipSwitches(ctx, "cloud provider off while a LoadBalancer exists", false, map[string]bool{cloudProviderSwitch.label: true}, cloudProviderSwitch); err != nil {
		return err
	}
	if err := r.flipSwitches(ctx, "cloud provider on", true, nil, cloudProviderSwitch); err != nil {
		return err
	}
	if err := kc.CoreV1().Services("default").Delete(ctx, guardService, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// OCCM must still be there to release the Octavia LB (finalizer).
	return waitFor(ctx, 10*time.Minute, "LoadBalancer service "+guardService+" deleted", func() (bool, error) {
		_, err := kc.CoreV1().Services("default").Get(ctx, guardService, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
}

const zincatiConfig = "/etc/zincati/config.d/90-magnum-updates.toml"

// osAutoUpgradeCycle proves the OS auto-upgrade switch reaches EVERY node
// (workers included — a label reconfigure used to re-fire masters only).
func (r *runner) osAutoUpgradeCycle(ctx context.Context) error {
	if r.cfg.sshUser == "ubuntu" {
		r.log("toggle-os-autoupgrade: SKIP on Ubuntu (zincati is Fedora CoreOS only)")
		return nil
	}
	for _, on := range []bool{true, false} {
		if err := r.runMutation(ctx, fmt.Sprintf("os_autoupgrade_enabled=%v", on), false, func() error {
			return r.patchClusterLabels(ctx, map[string]string{"os_autoupgrade_enabled": fmt.Sprint(on)}, nil)
		}); err != nil {
			return err
		}
		if err := r.assertZincatiEverywhere(ctx, on); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) assertZincatiEverywhere(ctx context.Context, enabled bool) error {
	var nodes []nodeAddr
	for _, role := range []string{"master", "worker"} {
		ns, err := r.nodesByRole(ctx, role)
		if err != nil {
			return err
		}
		nodes = append(nodes, ns...)
	}
	want := fmt.Sprintf("enabled = %v", enabled)
	for _, n := range nodes {
		out, err := r.nodeExec(ctx, n, "sudo cat "+zincatiConfig)
		if err != nil {
			return fmt.Errorf("os-autoupgrade: read %s on %s: %w", zincatiConfig, n.name, err)
		}
		if !strings.Contains(out, want) {
			return fmt.Errorf("os-autoupgrade: %s on %s lacks %q:\n%s", zincatiConfig, n.name, want, out)
		}
	}
	r.log("os-autoupgrade: %q on all %d nodes", want, len(nodes))
	return nil
}

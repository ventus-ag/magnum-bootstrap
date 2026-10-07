package main

import (
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/clusters"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ventus-ag/magnum-bootstrap/internal/module/kubecommon"
)

func TestComponentArgsMatchReconcilerContract(t *testing.T) {
	if kubeFilesDir != kubecommon.KubeFilesDir {
		t.Fatalf("kubeFilesDir %q drifted from kubecommon.KubeFilesDir %q", kubeFilesDir, kubecommon.KubeFilesDir)
	}
	labels := componentArgsLabels()
	files := map[string]string{e2eKubeFile: labels["kube_file_"+e2eKubeFile]}
	// The reconciler must accept the probe args once it has written the file.
	if err := kubecommon.CheckOptionFiles(kubecommon.KubeFilePaths(files), map[string]string{"kubeapi_options": labels["kubeapi_options"]}); err != nil {
		t.Fatalf("reconciler would reject the e2e args: %v", err)
	}
	if err := kubecommon.CheckOptionFiles(nil, map[string]string{"kubeapi_options": labels["kubeapi_options"]}); err == nil {
		t.Fatal("without the kube_file the args must be rejected (guards the missing-file check)")
	}
}

func TestLabelPatchOpts(t *testing.T) {
	got := labelPatchOpts(map[string]string{"b": "2", "a": "1"}, []string{"c"})
	want := []clusters.UpdateOpts{
		{Op: clusters.AddOp, Path: "/labels/a", Value: "1"},
		{Op: clusters.AddOp, Path: "/labels/b", Value: "2"},
		{Op: clusters.RemoveOp, Path: "/labels/c"},
	}
	if len(got) != len(want) {
		t.Fatalf("labelPatchOpts = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("op %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestNotReadyTolerationSeconds(t *testing.T) {
	secs := int64(301)
	got, err := notReadyTolerationSeconds([]corev1.Toleration{
		{Key: "node.kubernetes.io/unreachable", TolerationSeconds: &secs},
		{Key: "node.kubernetes.io/not-ready", TolerationSeconds: &secs},
	})
	if err != nil || got != 301 {
		t.Fatalf("got %d %v", got, err)
	}
	if _, err := notReadyTolerationSeconds(nil); err == nil {
		t.Fatal("missing toleration must error")
	}
}

func TestProbeCPURequest(t *testing.T) {
	node := func(cpu string) corev1.Node {
		return corev1.Node{Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}}}
	}
	if got := probeCPURequest([]corev1.Node{node("4"), node("1900m")}); got.MilliValue() != 190 {
		t.Errorf("10%% of the smallest node = %dm, want 190m", got.MilliValue())
	}
	if got := probeCPURequest([]corev1.Node{node("500m")}); got.MilliValue() != 100 {
		t.Errorf("floor = %dm, want 100m", got.MilliValue())
	}
	if packingProbePods*19 > 100 {
		t.Error("probe pods must fit on one node")
	}
}

func TestLeaseFresh(t *testing.T) {
	now := time.Now()
	holder, empty := "master-0", ""
	fresh := metav1.NewMicroTime(now.Add(-10 * time.Second))
	stale := metav1.NewMicroTime(now.Add(-2 * time.Minute))
	if !leaseFresh(&holder, &fresh, now) || leaseFresh(&holder, &stale, now) || leaseFresh(&empty, &fresh, now) || leaseFresh(nil, &fresh, now) {
		t.Fatal("leaseFresh misclassified")
	}
}

func TestStaleE2ECluster(t *testing.T) {
	now := time.Now()
	old := now.Add(-14 * time.Hour)
	for _, tc := range []struct {
		c    clusters.Cluster
		want bool
	}{
		{clusters.Cluster{Name: "recon-e2e-version-ladder-1", CreatedAt: old, Status: "UPDATE_COMPLETE"}, true},
		{clusters.Cluster{Name: "recon-e2e-smoke-2", CreatedAt: now.Add(-2 * time.Hour)}, false}, // live job
		{clusters.Cluster{Name: "customer-prod", CreatedAt: old}, false},                         // not ours
		{clusters.Cluster{Name: "recon-e2e-kept", CreatedAt: old, Labels: map[string]string{keepLabel: "true"}}, false},
		{clusters.Cluster{Name: "recon-e2e-going", CreatedAt: old, Status: "DELETE_IN_PROGRESS"}, false},
	} {
		if got := staleE2ECluster(tc.c, now); got != tc.want {
			t.Errorf("staleE2ECluster(%s) = %v, want %v", tc.c.Name, got, tc.want)
		}
	}
}

func TestLabelsApplied(t *testing.T) {
	live := map[string]string{"kubeapi_options": "--v=2", "auto_healing_controller": "magnum-auto-healer"}
	for _, tc := range []struct {
		name  string
		set   map[string]string
		unset []string
		want  bool
	}{
		{"set present", map[string]string{"kubeapi_options": "--v=2"}, nil, true},
		{"set stale value", map[string]string{"kubeapi_options": "--v=3"}, nil, false},
		{"set missing", map[string]string{"kube_file_x": "a"}, nil, false},
		{"unset gone", nil, []string{"kube_file_x"}, true},
		{"unset still there", nil, []string{"kubeapi_options"}, false},
	} {
		if got := labelsApplied(live, tc.set, tc.unset); got != tc.want {
			t.Errorf("%s: labelsApplied = %v, want %v", tc.name, got, tc.want)
		}
	}
}

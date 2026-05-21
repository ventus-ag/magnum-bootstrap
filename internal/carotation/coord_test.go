package carotation

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func fakeCoord(objects ...runtime.Object) *Coordinator {
	return &Coordinator{clientset: fake.NewSimpleClientset(objects...)}
}

func node(name, annotation string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if annotation != "" {
		n.Annotations = map[string]string{NodeAnnotation: annotation}
	}
	return n
}

func TestEnsureRotationAndReadDesiredPhase(t *testing.T) {
	c := fakeCoord()
	ctx := context.Background()

	// No ConfigMap yet → implicit prepare.
	if p, err := c.ReadDesiredPhase(ctx, "rot-1"); err != nil || p != PhasePrepare {
		t.Fatalf("ReadDesiredPhase before ensure = %q, %v; want prepare", p, err)
	}

	if err := c.EnsureRotation(ctx, "rot-1"); err != nil {
		t.Fatalf("EnsureRotation: %v", err)
	}
	if p, _ := c.ReadDesiredPhase(ctx, "rot-1"); p != PhasePrepare {
		t.Fatalf("desired after ensure = %q; want prepare", p)
	}
	// A different rotation reads as prepare (stale CM ignored).
	if p, _ := c.ReadDesiredPhase(ctx, "rot-2"); p != PhasePrepare {
		t.Fatalf("desired for other rotation = %q; want prepare", p)
	}
}

func TestAdvanceDesiredPhaseForwardOnly(t *testing.T) {
	c := fakeCoord()
	ctx := context.Background()
	if err := c.EnsureRotation(ctx, "rot-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.AdvanceDesiredPhase(ctx, "rot-1", PhaseCutover); err != nil {
		t.Fatal(err)
	}
	if p, _ := c.ReadDesiredPhase(ctx, "rot-1"); p != PhaseCutover {
		t.Fatalf("after advance = %q; want cutover", p)
	}
	// Advancing backward is a no-op.
	if err := c.AdvanceDesiredPhase(ctx, "rot-1", PhasePrepare); err != nil {
		t.Fatal(err)
	}
	if p, _ := c.ReadDesiredPhase(ctx, "rot-1"); p != PhaseCutover {
		t.Fatalf("after backward advance = %q; want cutover unchanged", p)
	}
}

func TestAllNodesReached(t *testing.T) {
	c := fakeCoord(
		node("m0", "cutover@rot-1"),
		node("m1", "prepare@rot-1"),
		node("w0", "prepare@rot-1"),
	)
	ctx := context.Background()

	ok, pending, err := c.AllNodesReached(ctx, "rot-1", PhasePrepare)
	if err != nil || !ok {
		t.Fatalf("AllNodesReached(prepare) = %v, pending=%v, err=%v; want true", ok, pending, err)
	}
	ok, pending, _ = c.AllNodesReached(ctx, "rot-1", PhaseCutover)
	if ok || len(pending) != 2 {
		t.Fatalf("AllNodesReached(cutover) = %v pending=%v; want false with 2 pending", ok, pending)
	}
	// Annotations from another rotation don't count.
	ok, _, _ = c.AllNodesReached(ctx, "rot-2", PhasePrepare)
	if ok {
		t.Fatal("nodes from rot-1 should not satisfy rot-2")
	}
}

func TestAllNodesReachedIgnoresTerminatingNode(t *testing.T) {
	terminating := node("dead", "")
	now := metav1.NewTime(time.Now())
	terminating.DeletionTimestamp = &now
	c := fakeCoord(node("m0", "prepare@rot-1"), terminating)

	ok, pending, err := c.AllNodesReached(context.Background(), "rot-1", PhasePrepare)
	if err != nil || !ok {
		t.Fatalf("terminating node should not block barrier: ok=%v pending=%v err=%v", ok, pending, err)
	}
}

func TestBarrierAdvancesWhenAllReached(t *testing.T) {
	c := fakeCoord(node("m0", "prepare@rot-1"))
	ctx := context.Background()
	if err := c.EnsureRotation(ctx, "rot-1"); err != nil {
		t.Fatal(err)
	}
	// Master should advance prepare→cutover because the only node reached prepare.
	err := c.Barrier(ctx, "rot-1", PhasePrepare, true, BarrierOptions{Poll: time.Millisecond, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Barrier: %v", err)
	}
	if p, _ := c.ReadDesiredPhase(ctx, "rot-1"); p != PhaseCutover {
		t.Fatalf("desired after barrier = %q; want cutover", p)
	}
}

func TestBarrierTimesOutWhenNodePending(t *testing.T) {
	// One node never reports cutover → master cannot advance → timeout.
	c := fakeCoord(node("m0", "cutover@rot-1"), node("w0", "prepare@rot-1"))
	ctx := context.Background()
	_ = c.EnsureRotation(ctx, "rot-1")
	_ = c.AdvanceDesiredPhase(ctx, "rot-1", PhaseCutover)
	err := c.Barrier(ctx, "rot-1", PhaseCutover, true, BarrierOptions{Poll: time.Millisecond, Timeout: 50 * time.Millisecond})
	if err == nil {
		t.Fatal("expected timeout waiting for pending node")
	}
}

func TestBarrierReturnsImmediatelyAfterFinalize(t *testing.T) {
	c := fakeCoord()
	if err := c.Barrier(context.Background(), "rot-1", PhaseFinalize, true, BarrierOptions{}); err != nil {
		t.Fatalf("Barrier after finalize should be a no-op: %v", err)
	}
}

func TestRestartLockMutualExclusionAndRelease(t *testing.T) {
	c := fakeCoord()
	ctx := context.Background()
	opts := LockOptions{DurationSeconds: 300, Poll: time.Millisecond, Timeout: time.Second}

	release, err := c.AcquireRestartLock(ctx, "m0", opts)
	if err != nil {
		t.Fatalf("m0 acquire: %v", err)
	}
	// While m0 holds it, m1 cannot acquire (short timeout → error).
	if _, err := c.AcquireRestartLock(ctx, "m1", LockOptions{DurationSeconds: 300, Poll: time.Millisecond, Timeout: 30 * time.Millisecond}); err == nil {
		t.Fatal("m1 should not acquire while m0 holds the lock")
	}
	release()
	// After release, m1 can acquire.
	release2, err := c.AcquireRestartLock(ctx, "m1", opts)
	if err != nil {
		t.Fatalf("m1 acquire after release: %v", err)
	}
	release2()
}

func TestRestartLockReentrant(t *testing.T) {
	c := fakeCoord()
	ctx := context.Background()
	opts := LockOptions{DurationSeconds: 300, Poll: time.Millisecond, Timeout: time.Second}
	r1, err := c.AcquireRestartLock(ctx, "m0", opts)
	if err != nil {
		t.Fatal(err)
	}
	// Same holder can re-acquire (renew) without blocking.
	r2, err := c.AcquireRestartLock(ctx, "m0", opts)
	if err != nil {
		t.Fatalf("reentrant acquire: %v", err)
	}
	r1()
	r2()
}

func TestReportStatusPatchesNodeAnnotation(t *testing.T) {
	c := fakeCoord(node("w0", ""))
	ctx := context.Background()
	if err := c.ReportStatus(ctx, "w0", PhaseCutover, "rot-1"); err != nil {
		t.Fatalf("ReportStatus: %v", err)
	}
	n, err := c.clientset.CoreV1().Nodes().Get(ctx, "w0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := n.Annotations[NodeAnnotation]; got != "cutover@rot-1" {
		t.Fatalf("node annotation = %q; want cutover@rot-1", got)
	}
}

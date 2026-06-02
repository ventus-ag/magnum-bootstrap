package carotation

import (
	"context"
	"strings"
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

func TestEnsureRotationSnapshotsParticipants(t *testing.T) {
	c := fakeCoord(node("m0", ""), node("w0", ""))
	ctx := context.Background()
	if err := c.EnsureRotation(ctx, "rot-1"); err != nil {
		t.Fatalf("EnsureRotation: %v", err)
	}
	got := c.readParticipants(ctx, "rot-1")
	want := []string{"m0", "w0"} // sorted snapshot of nodes present at start
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("participants = %v; want %v", got, want)
	}
	// A different rotation has no recorded participants (stale CM ignored).
	if p := c.readParticipants(ctx, "rot-2"); len(p) != 0 {
		t.Fatalf("participants for other rotation = %v; want none", p)
	}
}

func TestAllNodesReachedScopedToParticipants(t *testing.T) {
	c := fakeCoord(node("m0", ""), node("w0", ""))
	ctx := context.Background()
	if err := c.EnsureRotation(ctx, "rot-1"); err != nil {
		t.Fatal(err)
	}
	// A node that joins AFTER the snapshot (e.g. autoscaler/resize) is minted
	// with the new CA and never annotates; it must not block the barrier.
	if _, err := c.clientset.CoreV1().Nodes().Create(ctx, node("w-new", ""), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = c.ReportStatus(ctx, "m0", PhasePrepare, "rot-1")
	_ = c.ReportStatus(ctx, "w0", PhasePrepare, "rot-1")

	ok, pending, err := c.AllNodesReached(ctx, "rot-1", PhasePrepare)
	if err != nil || !ok {
		t.Fatalf("AllNodesReached = %v pending=%v err=%v; want true (new node excluded)", ok, pending, err)
	}
}

func TestAllNodesReachedSkipsDepartedParticipant(t *testing.T) {
	c := fakeCoord(node("m0", ""), node("w0", ""), node("doomed", ""))
	ctx := context.Background()
	if err := c.EnsureRotation(ctx, "rot-1"); err != nil {
		t.Fatal(err)
	}
	// "doomed" was a participant but has since left the cluster (deleted and
	// replaced with a new-CA node). It must not block the barrier forever.
	if err := c.clientset.CoreV1().Nodes().Delete(ctx, "doomed", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = c.ReportStatus(ctx, "m0", PhasePrepare, "rot-1")
	_ = c.ReportStatus(ctx, "w0", PhasePrepare, "rot-1")

	ok, pending, err := c.AllNodesReached(ctx, "rot-1", PhasePrepare)
	if err != nil || !ok {
		t.Fatalf("AllNodesReached = %v pending=%v err=%v; want true (departed participant skipped)", ok, pending, err)
	}
}

func TestNotReadyHintAndFormatPending(t *testing.T) {
	ready := node("ok", "")
	ready.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if got := formatPending("ok", ready); got != "ok" {
		t.Fatalf("formatPending(ready) = %q; want %q", got, "ok")
	}

	down := node("bad", "")
	down.Status.Conditions = []corev1.NodeCondition{{
		Type:               corev1.NodeReady,
		Status:             corev1.ConditionFalse,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-3 * time.Minute)),
	}}
	got := formatPending("bad", down)
	if !strings.HasPrefix(got, "bad(NotReady") {
		t.Fatalf("formatPending(notReady) = %q; want a NotReady hint", got)
	}

	// No reported conditions → bare name (cannot classify).
	if got := formatPending("unknown", node("unknown", "")); got != "unknown" {
		t.Fatalf("formatPending(no conditions) = %q; want %q", got, "unknown")
	}
}

func TestRunDeadlineUsesContextDeadline(t *testing.T) {
	// With a context deadline, the wait budget is the Heat window minus the
	// reserve, regardless of the (much smaller) fallback.
	dl := time.Now().Add(40 * time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), dl)
	defer cancel()
	got := runDeadline(ctx, 20*time.Minute)
	want := dl.Add(-barrierReserve)
	if d := got.Sub(want); d < -time.Second || d > time.Second {
		t.Fatalf("runDeadline with ctx deadline = %v; want ~%v", got, want)
	}

	// Without a context deadline, fall back to the fixed timeout.
	got = runDeadline(context.Background(), 50*time.Millisecond)
	if d := time.Until(got); d <= 0 || d > time.Minute {
		t.Fatalf("runDeadline fallback = %v from now; want ~50ms", d)
	}
}

func TestBarrierGivesUpAtContextDeadline(t *testing.T) {
	// A pending node would normally keep a master waiting for the full fallback
	// timeout (here 1h). A near-expired context deadline must cut the wait short.
	c := fakeCoord(node("m0", "cutover@rot-1"), node("w0", "prepare@rot-1"))
	_ = c.EnsureRotation(context.Background(), "rot-1")
	_ = c.AdvanceDesiredPhase(context.Background(), "rot-1", PhaseCutover)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Barrier(ctx, "rot-1", PhaseCutover, true, BarrierOptions{Poll: time.Millisecond, Timeout: time.Hour})
	if err == nil {
		t.Fatal("expected the barrier to give up at the context deadline")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("barrier ignored context deadline (waited %v)", elapsed)
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

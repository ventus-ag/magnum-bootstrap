package carotation

import (
	"context"
	"fmt"
	"strings"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

const (
	// CoordNamespace holds the rotation coordination objects.
	CoordNamespace = "kube-system"
	// ConfigMapName is the desired-phase ConfigMap (read by all nodes, written
	// by masters).
	ConfigMapName = "magnum-ca-rotation"
	// LeaseName serializes control-plane restarts across masters.
	LeaseName = "magnum-ca-rotation-restart"
	// NodeAnnotation carries each node's reported "<phase>@<rotationId>".
	NodeAnnotation = "magnum.openstack.org/ca-rotation"
	// rbacRoleName names the Role/RoleBinding granting nodes ConfigMap reads.
	rbacRoleName = "magnum-ca-rotation-reader"

	keyRotationID   = "rotationId"
	keyDesiredPhase = "desiredPhase"
)

// Coordinator wraps a Kubernetes client used to coordinate the rotation. The
// client is rebuilt from the live ca.crt (via Reload) whenever node trust
// changes, so it always trusts whatever CA material is currently active.
type Coordinator struct {
	kubeconfigPath string
	caFile         string
	clientset      kubernetes.Interface
}

// NewCoordinator builds a coordinator from a kubeconfig, overriding its trust
// anchor with caFile so the client follows the bundle during rotation.
func NewCoordinator(kubeconfigPath, caFile string) (*Coordinator, error) {
	c := &Coordinator{kubeconfigPath: kubeconfigPath, caFile: caFile}
	if err := c.Reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// Reload rebuilds the underlying client so it reads the current ca.crt. Call it
// after any phase that changes the live trust anchor.
func (c *Coordinator) Reload() error {
	cfg, err := clientcmd.BuildConfigFromFlags("", c.kubeconfigPath)
	if err != nil {
		return fmt.Errorf("ca-rotation: build kube client config: %w", err)
	}
	if c.caFile != "" {
		cfg.TLSClientConfig.CAData = nil
		cfg.TLSClientConfig.CAFile = c.caFile
	}
	cfg.Timeout = 20 * time.Second
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("ca-rotation: build kube client: %w", err)
	}
	c.clientset = cs
	return nil
}

// ReportStatus records that nodeName has completed phase for rotationID, as an
// annotation on the node's own object.
func (c *Coordinator) ReportStatus(ctx context.Context, nodeName string, phase Phase, rotationID string) error {
	value := string(phase) + "@" + rotationID
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, NodeAnnotation, value)
	_, err := c.clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("ca-rotation: report status for node %s: %w", nodeName, err)
	}
	return nil
}

// EnsureRotation makes sure the desired-phase ConfigMap exists and is scoped to
// rotationID, starting at prepare. Masters call this before the first barrier.
func (c *Coordinator) EnsureRotation(ctx context.Context, rotationID string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cms := c.clientset.CoreV1().ConfigMaps(CoordNamespace)
		cm, err := cms.Get(ctx, ConfigMapName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = cms.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: CoordNamespace},
				Data:       map[string]string{keyRotationID: rotationID, keyDesiredPhase: string(PhasePrepare)},
			}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if cm.Data[keyRotationID] == rotationID {
			return nil // already scoped to this rotation
		}
		// Stale ConfigMap from an earlier rotation — reset to this rotation.
		cm.Data = map[string]string{keyRotationID: rotationID, keyDesiredPhase: string(PhasePrepare)}
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

// EnsureNodeReadRBAC grants the system:nodes group read access to the single
// coordination ConfigMap, so workers (authenticated as system:node:<name>) can
// read the desired phase. It is idempotent and masters-only.
func (c *Coordinator) EnsureNodeReadRBAC(ctx context.Context) error {
	roles := c.clientset.RbacV1().Roles(CoordNamespace)
	if _, err := roles.Get(ctx, rbacRoleName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		_, err = roles.Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: rbacRoleName, Namespace: CoordNamespace},
			Rules: []rbacv1.PolicyRule{{
				APIGroups:     []string{""},
				Resources:     []string{"configmaps"},
				ResourceNames: []string{ConfigMapName},
				Verbs:         []string{"get", "list", "watch"},
			}},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if err != nil {
		return err
	}

	bindings := c.clientset.RbacV1().RoleBindings(CoordNamespace)
	if _, err := bindings.Get(ctx, rbacRoleName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		_, err = bindings.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: rbacRoleName, Namespace: CoordNamespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: rbacRoleName},
			Subjects: []rbacv1.Subject{{
				Kind:     rbacv1.GroupKind,
				APIGroup: rbacv1.GroupName,
				Name:     "system:nodes",
			}},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if err != nil {
		return err
	}
	return nil
}

// ReadDesiredPhase returns the cluster's desired phase for rotationID. A missing
// or stale ConfigMap reads as PhasePrepare (the implicit starting phase).
func (c *Coordinator) ReadDesiredPhase(ctx context.Context, rotationID string) (Phase, error) {
	cm, err := c.clientset.CoreV1().ConfigMaps(CoordNamespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return PhasePrepare, nil
	}
	if err != nil {
		return "", err
	}
	if cm.Data[keyRotationID] != rotationID {
		return PhasePrepare, nil
	}
	phase := Phase(cm.Data[keyDesiredPhase])
	if !phase.Valid() {
		return PhasePrepare, nil
	}
	return phase, nil
}

// AdvanceDesiredPhase moves the cluster desired phase forward to `to`. It is
// forward-only and idempotent: concurrent identical advances by multiple
// masters are harmless, and an attempt to move backward is a no-op.
func (c *Coordinator) AdvanceDesiredPhase(ctx context.Context, rotationID string, to Phase) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cms := c.clientset.CoreV1().ConfigMaps(CoordNamespace)
		cm, err := cms.Get(ctx, ConfigMapName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = cms.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: CoordNamespace},
				Data:       map[string]string{keyRotationID: rotationID, keyDesiredPhase: string(to)},
			}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if cm.Data[keyRotationID] != rotationID {
			cm.Data[keyRotationID] = rotationID
			cm.Data[keyDesiredPhase] = string(PhasePrepare)
		}
		if Phase(cm.Data[keyDesiredPhase]).AtLeast(to) {
			return nil
		}
		cm.Data[keyDesiredPhase] = string(to)
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

// AllNodesReached reports whether every node currently in the cluster has
// reported reaching at least `phase` for rotationID. It returns the list of
// nodes still pending so callers can log progress.
func (c *Coordinator) AllNodesReached(ctx context.Context, rotationID string, phase Phase) (bool, []string, error) {
	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, nil, err
	}
	var pending []string
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.DeletionTimestamp != nil {
			continue // node is terminating; don't block the barrier on it
		}
		reached, _ := parseNodeStatus(n.Annotations[NodeAnnotation], rotationID)
		if !reached.AtLeast(phase) {
			pending = append(pending, n.Name)
		}
	}
	return len(pending) == 0, pending, nil
}

// parseNodeStatus splits a "<phase>@<rotationId>" annotation. It returns an
// empty phase when the annotation is missing or belongs to a different rotation.
func parseNodeStatus(value, rotationID string) (Phase, bool) {
	at := strings.LastIndex(value, "@")
	if at < 0 {
		return "", false
	}
	if value[at+1:] != rotationID {
		return "", false
	}
	return Phase(value[:at]), true
}

// BarrierOptions tunes a barrier wait.
type BarrierOptions struct {
	Poll    time.Duration
	Timeout time.Duration
	Logf    func(format string, args ...any)
}

func (o BarrierOptions) withDefaults() BarrierOptions {
	if o.Poll <= 0 {
		o.Poll = 10 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 20 * time.Minute
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return o
}

// Barrier blocks until the cluster desired phase reaches the phase after
// `completed`. Masters drive advancement: once every node has reported reaching
// `completed`, a master moves the desired phase forward. Transient API errors
// are tolerated; the wait fails only on timeout. After the final phase there is
// no barrier and Barrier returns immediately.
func (c *Coordinator) Barrier(ctx context.Context, rotationID string, completed Phase, isMaster bool, opts BarrierOptions) error {
	next := completed.Next()
	if next == PhaseDone {
		return nil
	}
	opts = opts.withDefaults()
	deadline := time.Now().Add(opts.Timeout)
	for {
		desired, err := c.ReadDesiredPhase(ctx, rotationID)
		if err == nil && desired.AtLeast(next) {
			return nil
		}
		if err == nil && isMaster {
			ok, pending, listErr := c.AllNodesReached(ctx, rotationID, completed)
			if listErr == nil && ok {
				if advErr := c.AdvanceDesiredPhase(ctx, rotationID, next); advErr != nil {
					opts.Logf("ca-rotation: advance to %s failed (will retry): %v", next, advErr)
				} else {
					opts.Logf("ca-rotation: all nodes reached %s, advancing to %s", completed, next)
					continue
				}
			} else if listErr == nil {
				opts.Logf("ca-rotation: waiting for nodes to reach %s: %v", completed, pending)
			}
		}
		if err != nil {
			opts.Logf("ca-rotation: barrier read error (will retry): %v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ca-rotation: timed out waiting for cluster to reach %s", next)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.Poll):
		}
	}
}

// LockOptions tunes the restart lock acquisition.
type LockOptions struct {
	DurationSeconds int32
	Poll            time.Duration
	Timeout         time.Duration
	Logf            func(format string, args ...any)
}

func (o LockOptions) withDefaults() LockOptions {
	if o.DurationSeconds <= 0 {
		// The lease must outlive the longest legitimate single-master hold (a
		// full control-plane restart whose health wait can take several
		// minutes), or it could expire mid-restart and let a second master
		// restart too — breaking quorum. There is no renewal loop, so we err
		// long: a crashed holder is recovered after this window instead.
		o.DurationSeconds = 1200 // 20 minutes
	}
	if o.Poll <= 0 {
		o.Poll = 5 * time.Second
	}
	if o.Timeout <= 0 {
		// Long enough for several masters to restart serially before giving up.
		o.Timeout = 30 * time.Minute
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return o
}

// AcquireRestartLock takes the cluster-wide control-plane restart lock for
// holder, blocking until it is free or the timeout elapses. The returned
// release function frees the lock (best-effort). The lock self-expires after
// DurationSeconds so a crashed holder cannot wedge the cluster.
func (c *Coordinator) AcquireRestartLock(ctx context.Context, holder string, opts LockOptions) (func(), error) {
	opts = opts.withDefaults()
	deadline := time.Now().Add(opts.Timeout)
	for {
		acquired, err := c.tryAcquireLease(ctx, holder, opts.DurationSeconds)
		if err == nil && acquired {
			return func() { c.releaseLease(holder) }, nil
		}
		if err != nil {
			opts.Logf("ca-rotation: restart lock attempt error (will retry): %v", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("ca-rotation: timed out acquiring restart lock for %s", holder)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(opts.Poll):
		}
	}
}

func (c *Coordinator) tryAcquireLease(ctx context.Context, holder string, durationSeconds int32) (bool, error) {
	acquired := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		acquired = false
		leases := c.clientset.CoordinationV1().Leases(CoordNamespace)
		now := metav1.NewMicroTime(time.Now())
		lease, err := leases.Get(ctx, LeaseName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, createErr := leases.Create(ctx, &coordv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: CoordNamespace},
				Spec: coordv1.LeaseSpec{
					HolderIdentity:       ptr(holder),
					LeaseDurationSeconds: ptr(durationSeconds),
					AcquireTime:          &now,
					RenewTime:            &now,
				},
			}, metav1.CreateOptions{})
			if createErr != nil {
				return createErr
			}
			acquired = true
			return nil
		}
		if err != nil {
			return err
		}
		if leaseHeldByOther(lease, holder, time.Now()) {
			return nil // busy; not an error, caller retries after poll
		}
		lease.Spec.HolderIdentity = ptr(holder)
		lease.Spec.LeaseDurationSeconds = ptr(durationSeconds)
		if lease.Spec.AcquireTime == nil || derefHolder(lease) != holder {
			lease.Spec.AcquireTime = &now
		}
		lease.Spec.RenewTime = &now
		if _, updErr := leases.Update(ctx, lease, metav1.UpdateOptions{}); updErr != nil {
			return updErr
		}
		acquired = true
		return nil
	})
	return acquired, err
}

func (c *Coordinator) releaseLease(holder string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		leases := c.clientset.CoordinationV1().Leases(CoordNamespace)
		lease, err := leases.Get(ctx, LeaseName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if derefHolder(lease) != holder {
			return nil // someone else holds it; nothing to release
		}
		lease.Spec.HolderIdentity = ptr("")
		past := metav1.NewMicroTime(time.Now().Add(-time.Hour))
		lease.Spec.RenewTime = &past
		_, updErr := leases.Update(ctx, lease, metav1.UpdateOptions{})
		return updErr
	})
}

func leaseHeldByOther(lease *coordv1.Lease, holder string, now time.Time) bool {
	current := derefHolder(lease)
	if current == "" || current == holder {
		return false
	}
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	expiry := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	return now.Before(expiry)
}

func derefHolder(lease *coordv1.Lease) string {
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

func ptr[T any](v T) *T { return &v }

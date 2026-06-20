package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/certificates"
	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/clusters"
	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/nodegroups"
	appsv1 "k8s.io/api/apps/v1"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// k8sClient builds an admin kubernetes client for the live cluster the same way
// `openstack coe cluster config` does: generate a client key + CSR
// (CN=admin, O=system:masters — the built-in cluster-admin group), have the
// Magnum cert API sign it against the cluster CA, fetch the CA, and assemble a
// rest.Config. It is rebuilt on every call so a post-rotation smoke uses the
// freshly rotated CA + a freshly signed leaf.
func (r *runner) k8sClient(ctx context.Context) (*kubernetes.Clientset, error) {
	c, err := clusters.Get(ctx, r.magnum, r.cfg.clusterName).Extract()
	if err != nil {
		return nil, fmt.Errorf("get cluster: %w", err)
	}
	if c.APIAddress == "" {
		return nil, fmt.Errorf("cluster has no api_address yet")
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate client key: %w", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "admin", Organization: []string{"system:masters"}},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("create CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	signed, err := certificates.Create(ctx, r.magnum, certificates.CreateOpts{
		ClusterUUID: c.UUID,
		CSR:         string(csrPEM),
	}).Extract()
	if err != nil {
		return nil, fmt.Errorf("sign client CSR: %w", err)
	}
	ca, err := certificates.Get(ctx, r.magnum, c.UUID).Extract()
	if err != nil {
		return nil, fmt.Errorf("fetch cluster CA: %w", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cfg := &rest.Config{
		Host: c.APIAddress,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:   []byte(ca.PEM),
			CertData: []byte(signed.PEM),
			KeyData:  keyPEM,
		},
		Timeout: 30 * time.Second,
	}
	return kubernetes.NewForConfig(cfg)
}

// smokeCore waits until every node is Ready and prints the kube-system pods.
func (r *runner) smokeCore(ctx context.Context) error {
	r.log("smoke: nodes Ready + system pods")
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Minute)
	for {
		nodes, lerr := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if lerr == nil && len(nodes.Items) > 0 && allNodesReady(nodes.Items) {
			r.log("all %d node(s) Ready", len(nodes.Items))
			r.dumpNodes(nodes.Items)
			break
		}
		if time.Now().After(deadline) {
			// Dump what we have so the failure is self-explanatory: which
			// nodes are NotReady and the kubelet's reason (e.g. CNI not
			// initialized) instead of an opaque "timeout".
			r.log("nodes NOT all Ready after timeout — last observed state:")
			if lerr != nil {
				r.log("  (node list error: %v)", lerr)
			} else {
				r.dumpNodes(nodes.Items)
			}
			if pods, perr := kc.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{}); perr == nil {
				r.dumpPods("kube-system", pods.Items)
			}
			return fmt.Errorf("nodes not all Ready within timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	pods, err := kc.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list kube-system pods: %w", err)
	}
	r.dumpPods("kube-system", pods.Items)
	return nil
}

// dumpNodes prints a wide, kubectl-style node table: status (with the kubelet
// reason when NotReady), roles, kubelet version, and internal IP — enough to
// diagnose a stuck node-add without a separate kubectl session.
func (r *runner) dumpNodes(nodes []corev1.Node) {
	r.log("  %-46s %-26s %-14s %-12s %s", "NODE", "STATUS", "ROLES", "VERSION", "INTERNAL-IP")
	for _, n := range nodes {
		r.log("  %-46s %-26s %-14s %-12s %s",
			n.Name, nodeStatusStr(n), nodeRoles(n), n.Status.NodeInfo.KubeletVersion, nodeInternalIP(n))
	}
}

// dumpPods prints a wide pod table with readiness (ready/total containers),
// status, and restart count — restarts and not-Ready containers are the first
// signal a pod is crash-looping (e.g. flannel 401ing on a split SA key).
func (r *runner) dumpPods(ns string, pods []corev1.Pod) {
	r.log("%s pods (%d):", ns, len(pods))
	r.log("  %-56s %-7s %-22s %s", "NAME", "READY", "STATUS", "RESTARTS")
	for _, p := range pods {
		ready, total, restarts := 0, len(p.Spec.Containers), int32(0)
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
		}
		r.log("  %-56s %-7s %-22s %d", p.Name, fmt.Sprintf("%d/%d", ready, total), podStatusStr(p), restarts)
	}
}

// Smoke workload object names (shared across create, wait, resize, cleanup).
const (
	smokePVC    = "e2e-pvc"
	smokeDeploy = "e2e-web"
	smokeSvc    = "e2e-lb"
)

// smokeCloudIntegration is the payoff of the real-OpenStack tier: it proves the
// cloud controller manager (OCCM, via an Octavia LoadBalancer Service) and
// Cinder CSI (a dynamically provisioned PVC) actually work — neither of which
// the FCoS mock can fake. It goes beyond "provisioned": the LoadBalancer must
// actually serve HTTP through nginx, and the PVC must resize (online expansion).
//
//	PVC(1Gi) -> nginx Deployment mounts it -> LB Service -> bound? -> LB IP? ->
//	LB serves 200? -> resize PVC to 2Gi -> status.capacity reaches 2Gi? -> cleanup
func (r *runner) smokeCloudIntegration(ctx context.Context) error {
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	defer r.cleanupSmoke(ctx, kc)

	// --- Cinder CSI: dynamic PVC, mounted by the nginx pod. A consumer is needed
	// for a WaitForFirstConsumer StorageClass to bind, and the mount makes the
	// later resize an ONLINE expansion that updates status.capacity. ---
	r.log("smoke: Cinder CSI dynamic PVC (1Gi)")
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: smokePVC, Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	if _, err := kc.CoreV1().PersistentVolumeClaims("default").Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create PVC: %w", err)
	}

	// --- OCCM: nginx behind a LoadBalancer Service (Octavia). The Deployment
	// mounts the PVC so it both binds and can be resized online. ---
	r.log("smoke: nginx Deployment + OCCM LoadBalancer Service (Octavia)")
	if err := r.createLBWorkload(ctx, kc); err != nil {
		return err
	}

	// Pod scheduling + mount drives the bind (WaitForFirstConsumer).
	if err := r.waitPVCBound(ctx, kc); err != nil {
		return err
	}
	r.log("Cinder CSI PVC bound ✅")

	// LoadBalancer gets an external IP …
	ip, err := r.waitLBIngress(ctx, kc)
	if err != nil {
		return err
	}
	r.log("OCCM provisioned LoadBalancer IP %s — probing datapath", ip)
	// … and actually forwards traffic to nginx (proves the OCCM/Octavia datapath,
	// not merely that an address was allocated).
	if err := r.waitLBServes(ctx, ip); err != nil {
		return err
	}
	r.log("OCCM LoadBalancer serves HTTP 200 at %s ✅", ip)

	// --- Cinder CSI online volume expansion: 1Gi -> 2Gi. ---
	if err := r.resizePVC(ctx, kc); err != nil {
		return err
	}
	return nil
}

func (r *runner) waitPVCBound(ctx context.Context, kc *kubernetes.Clientset) error {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		pvc, err := kc.CoreV1().PersistentVolumeClaims("default").Get(ctx, smokePVC, metav1.GetOptions{})
		if err == nil && pvc.Status.Phase == corev1.ClaimBound {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("PVC did not bind within timeout — Cinder CSI/OCCM issue")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// createLBWorkload deploys an nginx Deployment behind a LoadBalancer Service.
// The pod mounts the e2e PVC at /data (a neutral path, so nginx still serves its
// default welcome page on "/" — the HTTP probe target) which both satisfies a
// WaitForFirstConsumer StorageClass and makes the PVC resize an online expansion.
func (r *runner) createLBWorkload(ctx context.Context, kc *kubernetes.Clientset) error {
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: smokeDeploy, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": smokeDeploy}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": smokeDeploy}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:         "nginx",
						Image:        "nginx:1.27-alpine",
						Ports:        []corev1.ContainerPort{{ContainerPort: 80}},
						VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
					}},
					Volumes: []corev1.Volume{{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: smokePVC},
						},
					}},
				},
			},
		},
	}
	if _, err := kc.AppsV1().Deployments("default").Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create nginx deployment: %w", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: smokeSvc, Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": smokeDeploy},
			Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt(80)}},
		},
	}
	if _, err := kc.CoreV1().Services("default").Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create LB service: %w", err)
	}
	return nil
}

func (r *runner) waitLBIngress(ctx context.Context, kc *kubernetes.Clientset) (string, error) {
	deadline := time.Now().Add(10 * time.Minute)
	for {
		svc, err := kc.CoreV1().Services("default").Get(ctx, "e2e-lb", metav1.GetOptions{})
		if err == nil {
			for _, ing := range svc.Status.LoadBalancer.Ingress {
				if ing.IP != "" {
					return ing.IP, nil
				}
				if ing.Hostname != "" {
					return ing.Hostname, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("LoadBalancer never got an external IP — OCCM/Octavia issue")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

// waitLBServes proves the OCCM/Octavia LoadBalancer actually forwards traffic
// (not just that an IP was allocated): it GETs http://<ip>/ until nginx answers
// 200. The datapath (member pool, health monitor) warms up a little after the IP
// is assigned, and the pod must first become Ready, so it retries.
func (r *runner) waitLBServes(ctx context.Context, ip string) error {
	url := "http://" + ip + "/"
	deadline := time.Now().Add(5 * time.Minute)
	var lastErr error
	for {
		if err := httpProbeOK(ctx, url, 15*time.Second); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("LoadBalancer %s never served HTTP 200 — OCCM datapath issue (last: %v)", ip, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// resizePVC exercises Cinder CSI online volume expansion: it grows the e2e PVC
// from 1Gi to 2Gi (get-modify-update on spec.resources.requests.storage) and
// waits for status.capacity to actually reach 2Gi. Because the PVC is mounted by
// the nginx pod this is an online expansion (ControllerExpand + NodeExpand) that
// updates status.capacity. A StorageClass without AllowVolumeExpansion would
// never converge, so that is checked first with a clear failure message.
func (r *runner) resizePVC(ctx context.Context, kc *kubernetes.Clientset) error {
	if err := r.ensureExpandableDefaultSC(ctx, kc); err != nil {
		return err
	}
	const want = "2Gi"
	wantQ := resource.MustParse(want)
	r.log("smoke: resize PVC %s 1Gi → %s (online)", smokePVC, want)

	pvc, err := kc.CoreV1().PersistentVolumeClaims("default").Get(ctx, smokePVC, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get PVC for resize: %w", err)
	}
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = wantQ
	if _, err := kc.CoreV1().PersistentVolumeClaims("default").Update(ctx, pvc, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("request PVC resize: %w", err)
	}

	deadline := time.Now().Add(5 * time.Minute)
	lastCap := "unset"
	for {
		cur, gerr := kc.CoreV1().PersistentVolumeClaims("default").Get(ctx, smokePVC, metav1.GetOptions{})
		if gerr == nil {
			if got, ok := cur.Status.Capacity[corev1.ResourceStorage]; ok {
				lastCap = got.String()
				if got.Cmp(wantQ) >= 0 {
					r.log("Cinder CSI PVC resized to %s ✅", lastCap)
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("PVC %s status.capacity=%s never reached %s — Cinder CSI volume expansion issue", smokePVC, lastCap, want)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// ensureExpandableDefaultSC verifies the cluster's default StorageClass allows
// volume expansion, so the resize step fails fast with a clear reason instead of
// timing out. The PVC uses the default SC (no storageClassName set).
func (r *runner) ensureExpandableDefaultSC(ctx context.Context, kc *kubernetes.Clientset) error {
	scs, err := kc.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list storage classes: %w", err)
	}
	for _, sc := range scs.Items {
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] != "true" {
			continue
		}
		if sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion {
			return fmt.Errorf("default StorageClass %q has allowVolumeExpansion != true — cannot test PVC resize", sc.Name)
		}
		r.log("default StorageClass %q allows volume expansion", sc.Name)
		return nil
	}
	return fmt.Errorf("no default StorageClass found — cannot test dynamic PVC resize")
}

func (r *runner) cleanupSmoke(ctx context.Context, kc *kubernetes.Clientset) {
	r.log("cleaning up smoke workloads")
	_ = kc.CoreV1().Services("default").Delete(ctx, smokeSvc, metav1.DeleteOptions{})
	_ = kc.AppsV1().Deployments("default").Delete(ctx, smokeDeploy, metav1.DeleteOptions{})
	_ = kc.CoreV1().PersistentVolumeClaims("default").Delete(ctx, smokePVC, metav1.DeleteOptions{})
}

// httpProbeOK does a single GET with a short timeout, returning nil only on a
// 200. Used by the LoadBalancer datapath probe loop (a short per-attempt timeout
// avoids a long hang on an allocated-but-not-yet-wired VIP).
func httpProbeOK(ctx context.Context, url string, timeout time.Duration) error {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// triggerCARotate fires a Magnum CA rotation (PATCH /certificates/{uuid}) with
// no wait — runMutation owns settle/wait/verify. The verify bundle re-runs the
// core smoke with a client rebuilt from the rotated CA, proving the new trust
// chain works end to end.
func (r *runner) triggerCARotate(ctx context.Context) error {
	c, err := clusters.Get(ctx, r.magnum, r.cfg.clusterName).Extract()
	if err != nil {
		return fmt.Errorf("get cluster: %w", err)
	}
	if err := certificates.Update(ctx, r.magnum, c.UUID).ExtractErr(); err != nil {
		return fmt.Errorf("trigger CA rotation: %w", err)
	}
	return nil
}

// verifyBundle is the post-op assertion suite run after every mutating op:
//   - smokeCore           — all nodes Ready (client rebuilt from live/rotated CA)
//   - verifyNodeCount     — k8s node count matches the sum of nodegroup desired
//     counts, and control-plane count matches the master nodegroup (catches a
//     resize/add/remove that didn't fully converge)
//   - verifySAConsistency — (disruptive ops only) SA-token trust consistent
//     across masters (split-trust regression)
//   - verifyNodepoolSchedulable — (when the extra nodepool exists) a pod pinned
//     to the nodepool actually schedules and runs
//
// Idempotency (a second reconcile = 0 changes) is NOT checked here — it needs
// re-triggering the reconciler on a node, which this tier can't do without a
// Heat op; it stays covered by the FCoS VM tier.
func (r *runner) verifyBundle(ctx context.Context, name string, disruptive bool) error {
	if err := r.verifyBundleInner(ctx, name, disruptive); err != nil {
		// Capture a node/pod diagnostic bundle at the moment of failure (mid-run),
		// before teardown wipes the cluster — this is the only chance to see why a
		// node went NotReady or a pod could not start.
		r.collectDiagnostics(ctx, "verify-"+name)
		return err
	}
	return nil
}

func (r *runner) verifyBundleInner(ctx context.Context, name string, disruptive bool) error {
	if err := r.smokeCore(ctx); err != nil {
		return fmt.Errorf("verify %s: %w", name, err)
	}
	if err := r.verifyNodeCount(ctx); err != nil {
		return fmt.Errorf("verify %s: %w", name, err)
	}
	if disruptive {
		if err := r.verifySAConsistency(ctx); err != nil {
			return fmt.Errorf("verify %s: %w", name, err)
		}
	}
	if r.nodepoolActive {
		if err := r.verifyNodepoolSchedulable(ctx); err != nil {
			return fmt.Errorf("verify %s: %w", name, err)
		}
	}
	return nil
}

// verifyNodeCount asserts the live Kubernetes node count equals the sum of every
// nodegroup's desired NodeCount, and the control-plane node count equals the
// master nodegroup's. This is the general invariant that proves a resize (up OR
// down), add-nodepool, or master add/remove fully converged — the removed node
// actually left, the added node actually joined.
func (r *runner) verifyNodeCount(ctx context.Context) error {
	pages, err := nodegroups.List(r.magnum, r.cfg.clusterName, nil).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list nodegroups: %w", err)
	}
	ngs, err := nodegroups.ExtractNodeGroups(pages)
	if err != nil {
		return fmt.Errorf("extract nodegroups: %w", err)
	}
	wantTotal, wantCP := 0, 0
	for _, ng := range ngs {
		wantTotal += ng.NodeCount
		if ng.Role == "master" {
			wantCP += ng.NodeCount
		}
	}

	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}

	// Heat reaches UPDATE_COMPLETE when the OpenStack VM is gone, but on a
	// scaledown the removed node's Kubernetes Node object lingers until the
	// cloud-node-lifecycle controller (OCCM) deletes it — a lag of up to a
	// minute or more. The nodegroup desired counts are already final, so poll
	// the live node list until it converges instead of reading it once.
	deadline := time.Now().Add(10 * time.Minute)
	var lastErr error
	for {
		nodes, lerr := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if lerr != nil {
			lastErr = fmt.Errorf("list nodes: %w", lerr)
		} else {
			gotTotal := len(nodes.Items)
			gotCP := 0
			for _, n := range nodes.Items {
				if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
					gotCP++
				}
			}
			switch {
			case gotTotal != wantTotal:
				lastErr = fmt.Errorf("node count mismatch: nodegroups want %d, cluster has %d", wantTotal, gotTotal)
			case gotCP != wantCP:
				lastErr = fmt.Errorf("control-plane count mismatch: master nodegroup wants %d, cluster has %d", wantCP, gotCP)
			default:
				r.log("node count OK: %d total (%d control-plane) matches nodegroups ✅", gotTotal, gotCP)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		r.log("node count not converged yet (%v) — waiting", lastErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// verifyNodepoolSchedulable proves the extra nodepool's nodes are real, usable
// workers: it schedules a pod pinned (nodeSelector) to the nodepool's node label
// (magnum.openstack.org/nodegroup=<name>, set by the reconciler) and asserts it
// reaches Running on a nodepool node.
func (r *runner) verifyNodepoolSchedulable(ctx context.Context) error {
	name := r.nodepoolName()
	r.log("verify: nodepool %q schedulable", name)
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	const podName = "e2e-np-probe"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeSelector:  map[string]string{"magnum.openstack.org/nodegroup": name},
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "pause",
				Image: "registry.k8s.io/pause:3.9",
			}},
		},
	}
	_ = kc.CoreV1().Pods("default").Delete(ctx, podName, metav1.DeleteOptions{})
	if _, err := kc.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create nodepool probe pod: %w", err)
	}
	defer func() {
		_ = kc.CoreV1().Pods("default").Delete(ctx, podName, metav1.DeleteOptions{})
	}()

	deadline := time.Now().Add(5 * time.Minute)
	for {
		p, gerr := kc.CoreV1().Pods("default").Get(ctx, podName, metav1.GetOptions{})
		if gerr == nil && p.Status.Phase == corev1.PodRunning {
			r.log("nodepool %q probe pod Running on node %s ✅", name, p.Spec.NodeName)
			return nil
		}
		if time.Now().After(deadline) {
			phase := "unknown"
			if gerr == nil {
				phase = string(p.Status.Phase)
			}
			return fmt.Errorf("nodepool %q probe pod did not reach Running (phase=%s) — nodepool not schedulable", name, phase)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func allNodesReady(nodes []corev1.Node) bool {
	for _, n := range nodes {
		ready := false
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			return false
		}
	}
	return true
}

// nodeStatusStr is "Ready", or "NotReady(<reason>)" carrying the kubelet's Ready
// condition reason (e.g. KubeletNotReady) so a stuck node explains itself.
func nodeStatusStr(n corev1.Node) string {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			if cond.Status == corev1.ConditionTrue {
				return "Ready"
			}
			if cond.Reason != "" {
				return "NotReady(" + cond.Reason + ")"
			}
			return "NotReady"
		}
	}
	return "Unknown"
}

// nodeRoles joins the node-role.kubernetes.io/<role> labels ("control-plane",
// "worker"), matching kubectl's ROLES column.
func nodeRoles(n corev1.Node) string {
	const prefix = "node-role.kubernetes.io/"
	var roles []string
	for k := range n.Labels {
		if role, ok := strings.CutPrefix(k, prefix); ok && role != "" {
			roles = append(roles, role)
		}
	}
	if len(roles) == 0 {
		return "worker"
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

func nodeInternalIP(n corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return "-"
}

// podStatusStr mimics kubectl's STATUS column: a waiting/terminated container
// reason (CrashLoopBackOff, CreateContainerError, …) surfaces ahead of the bare
// pod phase, since that reason is the actual diagnostic.
func podStatusStr(p corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			return w.Reason
		}
		if t := cs.State.Terminated; t != nil && t.Reason != "" {
			return t.Reason
		}
	}
	return string(p.Status.Phase)
}

// verifySAConsistency proves every apiserver shares the same service-account
// keys after a rotation + node-add. It creates a throwaway ServiceAccount,
// mints a bound token (signed by whichever apiserver answers the TokenRequest),
// then makes many authenticated probes THROUGH the API load balancer, each on a
// fresh connection so they spread across masters.
//
// A probe that returns 401 Unauthorized means an apiserver rejected the token —
// i.e. it holds a different service-account key (the split-trust bug a
// post-rotation node-add can introduce). 403 Forbidden (or 200) is success: the
// token was accepted; only RBAC denied the request. Transient network errors
// are ignored — only an explicit 401 fails the test.
func (r *runner) verifySAConsistency(ctx context.Context) error {
	r.log("verify: service-account token trust is consistent across masters")
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	c, err := clusters.Get(ctx, r.magnum, r.cfg.clusterName).Extract()
	if err != nil {
		return fmt.Errorf("get cluster: %w", err)
	}
	ca, err := certificates.Get(ctx, r.magnum, c.UUID).Extract()
	if err != nil {
		return fmt.Errorf("fetch cluster CA: %w", err)
	}

	const saName = "e2e-sa-check"
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: "default"},
	}
	if _, err := kc.CoreV1().ServiceAccounts("default").Create(ctx, sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create service account: %w", err)
	}
	defer func() {
		_ = kc.CoreV1().ServiceAccounts("default").Delete(ctx, saName, metav1.DeleteOptions{})
	}()

	tok, err := kc.CoreV1().ServiceAccounts("default").CreateToken(ctx, saName, &authnv1.TokenRequest{}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("mint SA token: %w", err)
	}

	const probes = 20
	accepted := 0
	for i := 0; i < probes; i++ {
		// Fresh client per probe → fresh TLS connection → the API LB spreads
		// probes across masters, so a single stale-keyed master gets hit.
		probeKC, err := kubernetes.NewForConfig(&rest.Config{
			Host:            c.APIAddress,
			BearerToken:     tok.Status.Token,
			TLSClientConfig: rest.TLSClientConfig{CAData: []byte(ca.PEM)},
			Timeout:         15 * time.Second,
		})
		if err != nil {
			return fmt.Errorf("build probe client: %w", err)
		}
		_, gerr := probeKC.CoreV1().Namespaces().Get(ctx, "default", metav1.GetOptions{})
		switch {
		case gerr == nil || apierrors.IsForbidden(gerr):
			accepted++ // token authenticated; RBAC denial is fine
		case apierrors.IsUnauthorized(gerr):
			return fmt.Errorf("SA token rejected (401) on probe %d/%d — an apiserver holds a different service-account key (rotation/resize SA-key split)", i+1, probes)
		default:
			r.log("  probe %d/%d transient error (ignored): %v", i+1, probes, gerr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	r.log("SA token accepted on %d/%d probes across masters ✅", accepted, probes)
	return nil
}

// collectDiagnostics gathers a node/pod failure bundle straight from the
// Kubernetes API — this tier has no SSH to nodes, so node-level state comes from
// node conditions, Warning events, and the logs of pods that won't start. It
// writes the full bundle to DIAG_DIR (uploaded as a CI artifact) and a one-line
// summary to the run log. Best-effort: never returns an error and is safe to
// call mid-run at any failure point, before teardown wipes the cluster.
func (r *runner) collectDiagnostics(ctx context.Context, reason string) {
	kc, err := r.k8sClient(ctx)
	if err != nil {
		r.err("diagnostics: cannot reach cluster API (%v)", err)
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "==== e2e node diagnostics ====\nreason:   %s\nscenario: %s\ncluster:  %s\ntime:     %s\n\n",
		reason, r.cfg.scenario, r.cfg.clusterName, time.Now().UTC().Format("2006-01-02T15:04:05Z"))

	// Nodes — with full condition messages for any that are not Ready.
	var notReady []string
	if nodes, nerr := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); nerr == nil {
		fmt.Fprintf(&b, "-- nodes (%d) --\n%-46s %-26s %-14s %-12s %s\n",
			len(nodes.Items), "NODE", "STATUS", "ROLES", "VERSION", "INTERNAL-IP")
		for _, n := range nodes.Items {
			st := nodeStatusStr(n)
			fmt.Fprintf(&b, "%-46s %-26s %-14s %-12s %s\n",
				n.Name, st, nodeRoles(n), n.Status.NodeInfo.KubeletVersion, nodeInternalIP(n))
			if st != "Ready" {
				notReady = append(notReady, n.Name)
				for _, c := range n.Status.Conditions {
					if c.Status != corev1.ConditionTrue && (c.Type == corev1.NodeReady || strings.TrimSpace(c.Message) != "") {
						fmt.Fprintf(&b, "    %s=%s %s: %s\n", c.Type, c.Status, c.Reason, strings.TrimSpace(c.Message))
					}
				}
			}
		}
	} else {
		fmt.Fprintf(&b, "-- nodes: list error: %v --\n", nerr)
	}

	// Problem pods (all namespaces): not Running/Succeeded, a container not ready,
	// or any restarts. These are what to pull logs from.
	var problems []corev1.Pod
	if pods, perr := kc.CoreV1().Pods("").List(ctx, metav1.ListOptions{}); perr == nil {
		fmt.Fprintf(&b, "\n-- problem pods --\n%-20s %-52s %-7s %-22s %-9s %s\n",
			"NAMESPACE", "NAME", "READY", "STATUS", "RESTARTS", "NODE")
		for _, p := range pods.Items {
			ready, total, restarts := 0, len(p.Spec.Containers), int32(0)
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Ready {
					ready++
				}
				restarts += cs.RestartCount
			}
			if (p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodSucceeded) && ready == total && restarts == 0 {
				continue
			}
			fmt.Fprintf(&b, "%-20s %-52s %-7s %-22s %-9d %s\n",
				p.Namespace, p.Name, fmt.Sprintf("%d/%d", ready, total), podStatusStr(p), restarts, p.Spec.NodeName)
			problems = append(problems, p)
		}
		if len(problems) == 0 {
			fmt.Fprintf(&b, "(none)\n")
		}
	}

	// Recent Warning events (all namespaces) — FailedCreatePodSandBox,
	// FailedScheduling, image-pull errors etc. are the direct "pod can't start" signal.
	if evs, eerr := kc.CoreV1().Events("").List(ctx, metav1.ListOptions{}); eerr == nil {
		type ev struct {
			t time.Time
			s string
		}
		var warns []ev
		for _, e := range evs.Items {
			if e.Type != corev1.EventTypeWarning {
				continue
			}
			when := e.LastTimestamp.Time
			if when.IsZero() {
				when = e.EventTime.Time
			}
			warns = append(warns, ev{when, fmt.Sprintf("%dx %s %s/%s %s: %s",
				e.Count, e.InvolvedObject.Kind, e.InvolvedObject.Namespace, e.InvolvedObject.Name,
				e.Reason, strings.TrimSpace(e.Message))})
		}
		sort.Slice(warns, func(i, j int) bool { return warns[i].t.After(warns[j].t) })
		fmt.Fprintf(&b, "\n-- recent Warning events (%d total, newest first, capped 40) --\n", len(warns))
		for i, w := range warns {
			if i >= 40 {
				break
			}
			fmt.Fprintf(&b, "%s  %s\n", w.t.UTC().Format("15:04:05"), w.s)
		}
	}

	// Tail logs of problem pods (current + previous when a container has restarted).
	fmt.Fprintf(&b, "\n-- problem pod logs (tail 60) --\n")
	for i, p := range problems {
		if i >= 12 {
			fmt.Fprintf(&b, "\n(… %d more problem pods omitted)\n", len(problems)-i)
			break
		}
		for _, c := range p.Spec.Containers {
			fmt.Fprintf(&b, "\n### %s/%s [%s] on %s\n", p.Namespace, p.Name, c.Name, p.Spec.NodeName)
			b.WriteString(podLogTail(ctx, kc, p.Namespace, p.Name, c.Name, false))
			if containerRestarted(p, c.Name) {
				fmt.Fprintf(&b, "--- previous (crashed) instance ---\n")
				b.WriteString(podLogTail(ctx, kc, p.Namespace, p.Name, c.Name, true))
			}
		}
	}

	r.writeDiagFile(reason, b.String())
	r.log("diagnostics: %d node(s) NotReady %v, %d problem pod(s)", len(notReady), notReady, len(problems))
}

// podLogTail returns the last lines of one container's log (previous=crashed
// instance), or an inline error note — never fails the caller.
func podLogTail(ctx context.Context, kc *kubernetes.Clientset, ns, pod, container string, previous bool) string {
	lines := int64(60)
	req := kc.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: container, TailLines: &lines, Previous: previous,
	})
	out, err := req.DoRaw(ctx)
	if err != nil {
		return fmt.Sprintf("(log fetch error: %v)\n", err)
	}
	if len(out) == 0 {
		return "(no log output)\n"
	}
	return string(out)
}

func containerRestarted(p corev1.Pod, name string) bool {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == name && cs.RestartCount > 0 {
			return true
		}
	}
	return false
}

// writeDiagFile writes the bundle to DIAG_DIR (default ./e2e-diagnostics) for CI
// artifact upload, falling back to stdout so the data survives even if the dir
// is not writable.
func (r *runner) writeDiagFile(reason, content string) {
	dir := os.Getenv("DIAG_DIR")
	if dir == "" {
		dir = "e2e-diagnostics"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.err("diagnostics: mkdir %s: %v — dumping inline", dir, err)
		fmt.Println(content)
		return
	}
	fn := filepath.Join(dir, fmt.Sprintf("%s-%s-%s.log",
		r.cfg.clusterName, sanitizeFilename(reason), time.Now().UTC().Format("150405")))
	if err := os.WriteFile(fn, []byte(content), 0o644); err != nil {
		r.err("diagnostics: write %s: %v — dumping inline", fn, err)
		fmt.Println(content)
		return
	}
	r.log("diagnostics bundle written: %s", fn)
}

func sanitizeFilename(s string) string {
	var out strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out.WriteRune(c)
		default:
			out.WriteRune('-')
		}
	}
	return out.String()
}

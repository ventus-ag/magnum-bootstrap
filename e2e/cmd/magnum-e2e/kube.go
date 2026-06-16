package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/certificates"
	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/clusters"
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
			for _, n := range nodes.Items {
				r.log("  node %s: %s", n.Name, nodeReadyStr(n))
			}
			break
		}
		if time.Now().After(deadline) {
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
	r.log("kube-system pods (%d):", len(pods.Items))
	for _, p := range pods.Items {
		r.log("  %-50s %s", p.Name, p.Status.Phase)
	}
	return nil
}

// smokeCloudIntegration is the payoff of the real-OpenStack tier: it proves the
// cloud controller manager (OCCM, via an Octavia LoadBalancer Service) and
// Cinder CSI (a dynamically provisioned PVC) actually work — neither of which
// the FCoS mock can fake.
func (r *runner) smokeCloudIntegration(ctx context.Context) error {
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	defer r.cleanupSmoke(ctx, kc)

	// --- Cinder CSI: dynamic PVC binds ---
	r.log("smoke: Cinder CSI dynamic PVC")
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-pvc", Namespace: "default"},
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
	if err := r.waitPVCBound(ctx, kc); err != nil {
		return err
	}
	r.log("Cinder CSI PVC bound ✅")

	// --- OCCM: LoadBalancer Service gets an external IP (Octavia) ---
	r.log("smoke: OCCM LoadBalancer Service (Octavia)")
	if err := r.createLBWorkload(ctx, kc); err != nil {
		return err
	}
	ip, err := r.waitLBIngress(ctx, kc)
	if err != nil {
		return err
	}
	r.log("OCCM provisioned LoadBalancer IP %s ✅", ip)
	return nil
}

func (r *runner) waitPVCBound(ctx context.Context, kc *kubernetes.Clientset) error {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		pvc, err := kc.CoreV1().PersistentVolumeClaims("default").Get(ctx, "e2e-pvc", metav1.GetOptions{})
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

func (r *runner) createLBWorkload(ctx context.Context, kc *kubernetes.Clientset) error {
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "e2e-web"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "e2e-web"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "agnhost",
					Image: "registry.k8s.io/e2e-test-images/agnhost:2.47",
					Args:  []string{"netexec", "--http-port=8080"},
					Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
				}}},
			},
		},
	}
	if _, err := kc.AppsV1().Deployments("default").Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create deployment: %w", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-lb", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": "e2e-web"},
			Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt(8080)}},
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

func (r *runner) cleanupSmoke(ctx context.Context, kc *kubernetes.Clientset) {
	r.log("cleaning up smoke workloads")
	_ = kc.CoreV1().Services("default").Delete(ctx, "e2e-lb", metav1.DeleteOptions{})
	_ = kc.AppsV1().Deployments("default").Delete(ctx, "e2e-web", metav1.DeleteOptions{})
	_ = kc.CoreV1().PersistentVolumeClaims("default").Delete(ctx, "e2e-pvc", metav1.DeleteOptions{})
}

// caRotateCluster triggers a Magnum CA rotation (PATCH /certificates/{uuid}),
// waits for the update to complete, then re-runs the core smoke with a client
// built from the rotated CA — proving the new trust chain works end to end.
func (r *runner) caRotateCluster(ctx context.Context) error {
	r.log("=== ca-rotate cluster ===")
	c, err := clusters.Get(ctx, r.magnum, r.cfg.clusterName).Extract()
	if err != nil {
		return fmt.Errorf("get cluster: %w", err)
	}
	if err := certificates.Update(ctx, r.magnum, c.UUID).ExtractErr(); err != nil {
		return fmt.Errorf("trigger CA rotation: %w", err)
	}
	if err := r.waitStatus(ctx, "UPDATE_COMPLETE"); err != nil {
		return err
	}
	return r.smokeCore(ctx)
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

func nodeReadyStr(n corev1.Node) string {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return string(cond.Status)
		}
	}
	return "Unknown"
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

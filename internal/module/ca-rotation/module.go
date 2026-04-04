package carotation

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	magnumapi "github.com/ventus-ag/magnum-bootstrap/internal/magnum"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "ca-rotation" }
func (Module) Dependencies() []string { return []string{"prereq-validation"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	rotationID := cfg.Trigger.CARotationID

	// No rotation requested — nothing to do.
	if rotationID == "" {
		return moduleapi.Result{}, nil
	}
	if cfg.Shared.TLSDisabled {
		return moduleapi.Result{}, nil
	}

	// Check if this rotation was already applied.
	lastRotationFile := "/var/lib/magnum/last_ca_rotation_id"
	if data, err := os.ReadFile(lastRotationFile); err == nil {
		if string(data) == rotationID {
			// Already rotated — no-op.
			return moduleapi.Result{}, nil
		}
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change
	certDir := "/etc/kubernetes/certs"

	if !req.Apply {
		changes = append(changes, host.Change{
			Action:  host.ActionReplace,
			Path:    certDir,
			Summary: fmt.Sprintf("rotate CA certificates (rotation_id=%s)", rotationID),
		})
		return moduleapi.Result{Changes: changes}, nil
	}

	// Validate service account keys are provided.
	if cfg.Shared.KubeServiceAccountKey == "" || cfg.Shared.KubeServiceAccountPrivateKey == "" {
		return moduleapi.Result{}, fmt.Errorf("ca-rotation: service account keys must be provided")
	}

	client := magnumapi.NewClient(
		cfg.Shared.AuthURL, cfg.Shared.MagnumURL,
		cfg.Shared.TrusteeUserID, cfg.Shared.TrusteePassword,
		cfg.Shared.TrustID, cfg.Shared.ClusterUUID,
		cfg.Shared.VerifyCA,
	)

	token, err := client.GetToken()
	if err != nil {
		return moduleapi.Result{}, fmt.Errorf("ca-rotation: keystone auth: %w", err)
	}

	// Fetch new CA cert.
	caPEM, err := client.FetchCACert(token)
	if err != nil {
		return moduleapi.Result{}, fmt.Errorf("ca-rotation: fetch CA: %w", err)
	}

	// Stage certs in a temporary directory before replacing.
	stageDir := fmt.Sprintf("/var/lib/magnum/ca-rotation/%s", rotationID)
	stageCertDir := filepath.Join(stageDir, "kubernetes-certs")
	if err := os.MkdirAll(stageCertDir, 0o700); err != nil {
		return moduleapi.Result{}, fmt.Errorf("ca-rotation: create staging dir: %w", err)
	}

	// Write CA cert to staging.
	if err := os.WriteFile(filepath.Join(stageCertDir, "ca.crt"), []byte(caPEM), 0o444); err != nil {
		return moduleapi.Result{}, err
	}

	if cfg.Role() == config.RoleMaster {
		cs, err := rotateMasterCerts(cfg, executor, client, token, stageCertDir)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)
	} else {
		cs, err := rotateWorkerCerts(cfg, executor, client, token, stageCertDir)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)
	}

	// Verify all staged certs exist before replacing.
	if err := verifyStagedCerts(stageCertDir, cfg.Role()); err != nil {
		return moduleapi.Result{}, fmt.Errorf("ca-rotation: staged cert verification failed: %w", err)
	}

	// Atomically replace certs: copy staged certs to the real cert dir.
	_ = executor.Run("cp", "-a", stageCertDir+"/.", certDir+"/")
	changes = append(changes, host.Change{Action: host.ActionReplace, Path: certDir, Summary: "replace certificates with rotated versions"})

	// Copy to etcd certs if master.
	if cfg.Role() == config.RoleMaster {
		etcdCertDir := "/etc/etcd/certs"
		_ = executor.Run("cp", "-a", certDir+"/.", etcdCertDir+"/")
		// Fix ownership.
		_ = executor.Run("chown", "-R", "kube:kube_etcd", certDir)
		_ = executor.Run("chown", "-R", "etcd:kube_etcd", etcdCertDir)
		changes = append(changes, host.Change{Action: host.ActionUpdate, Path: etcdCertDir, Summary: "update etcd certificates"})
	}

	// Write CA key for cert-manager if master and key is provided.
	if cfg.Role() == config.RoleMaster && cfg.Shared.CAKey != "" {
		change, err := executor.EnsureFile(certDir+"/ca.key", []byte(cfg.Shared.CAKey+"\n"), 0o400)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	// Update admin kubeconfig with new certs (master only).
	if cfg.Role() == config.RoleMaster {
		cs, err := updateAdminKubeconfig(cfg, executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)
	}

	// Restart services with new certs.
	cs, err := restartServices(cfg, executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, cs...)

	// Wait for services to be healthy after restart.
	if err := waitForHealthy(cfg, executor); err != nil {
		return moduleapi.Result{}, fmt.Errorf("ca-rotation: post-restart health check failed: %w", err)
	}

	// Patch all workloads to trigger rollout with new CA (master only).
	if cfg.Role() == config.RoleMaster {
		changes = append(changes, patchWorkloads(executor, rotationID)...)
	}

	// Record rotation ID.
	change, err := executor.EnsureFile(lastRotationFile, []byte(rotationID), 0o644)
	if err != nil {
		return moduleapi.Result{}, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Clean up staging directory on success.
	_ = os.RemoveAll(stageDir)

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{
			"caRotationId": rotationID,
			"role":         cfg.Role().String(),
		},
	}, nil
}

func rotateMasterCerts(cfg config.Config, executor *host.Executor, client *magnumapi.Client, token, stageCertDir string) ([]host.Change, error) {
	var changes []host.Change

	nodeIP := cfg.ResolveNodeIP()
	var sanIPs []string
	addIP := func(ip string) {
		if ip == "" {
			return
		}
		for _, existing := range sanIPs {
			if existing == ip {
				return
			}
		}
		sanIPs = append(sanIPs, ip)
	}
	addIP(nodeIP)
	addIP(cfg.Shared.KubeNodePublicIP)
	if cfg.Master != nil {
		addIP(cfg.Master.KubeAPIPublicAddress)
		addIP(cfg.Master.KubeAPIPrivateAddress)
		addIP(cfg.Master.EtcdLBVIP)
	}
	addIP("127.0.0.1")

	// Include Kubernetes service IP in SANs.
	if cfg.Shared.PortalNetworkCIDR != "" {
		if serviceIP := computeServiceIP(cfg.Shared.PortalNetworkCIDR); serviceIP != "" {
			addIP(serviceIP)
		}
	}

	sanDNSs := []string{"kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local"}
	if cfg.Master != nil && cfg.Master.MasterHostname != "" {
		sanDNSs = append(sanDNSs, cfg.Master.MasterHostname)
	}

	specs := []magnumapi.CertSpec{
		{Name: "server", CN: "kubernetes", SANIPs: sanIPs, SANDNSs: sanDNSs},
		{Name: "kubelet", CN: fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName), O: "system:nodes", SANIPs: sanIPs, SANDNSs: sanDNSs},
		{Name: "admin", CN: "admin", O: "system:masters"},
		{Name: "proxy", CN: "system:kube-proxy", O: "system:node-proxier"},
		{Name: "controller", CN: "system:kube-controller-manager", O: "system:kube-controller-manager"},
		{Name: "scheduler", CN: "system:kube-scheduler", O: "system:kube-scheduler"},
	}

	for _, spec := range specs {
		cs, err := generateAndWriteCert(executor, client, token, stageCertDir, spec)
		if err != nil {
			return nil, err
		}
		changes = append(changes, cs...)
	}

	// Write service account keys.
	if err := os.WriteFile(filepath.Join(stageCertDir, "service_account.key"),
		[]byte(cfg.Shared.KubeServiceAccountKey+"\n"), 0o440); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(stageCertDir, "service_account_private.key"),
		[]byte(cfg.Shared.KubeServiceAccountPrivateKey+"\n"), 0o440); err != nil {
		return nil, err
	}

	return changes, nil
}

func rotateWorkerCerts(cfg config.Config, executor *host.Executor, client *magnumapi.Client, token, stageCertDir string) ([]host.Change, error) {
	var changes []host.Change

	nodeIP := cfg.ResolveNodeIP()
	var sanIPs []string
	if nodeIP != "" {
		sanIPs = append(sanIPs, nodeIP)
	}
	sanDNSs := []string{cfg.Shared.InstanceName}

	specs := []magnumapi.CertSpec{
		{Name: "kubelet", CN: fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName), O: "system:nodes", SANIPs: sanIPs, SANDNSs: sanDNSs},
		{Name: "proxy", CN: "system:kube-proxy", O: "system:node-proxier"},
	}

	for _, spec := range specs {
		cs, err := generateAndWriteCert(executor, client, token, stageCertDir, spec)
		if err != nil {
			return nil, err
		}
		changes = append(changes, cs...)
	}

	return changes, nil
}

func generateAndWriteCert(executor *host.Executor, client *magnumapi.Client, token, certDir string, spec magnumapi.CertSpec) ([]host.Change, error) {
	var changes []host.Change

	keyPEM, csrPEM, err := magnumapi.GenerateKeyAndCSR(spec)
	if err != nil {
		return nil, fmt.Errorf("generate %s key/CSR: %w", spec.Name, err)
	}

	certPEM, err := client.SignCSR(token, csrPEM)
	if err != nil {
		return nil, fmt.Errorf("sign %s CSR: %w", spec.Name, err)
	}

	keyPath := filepath.Join(certDir, spec.Name+".key")
	certPath := filepath.Join(certDir, spec.Name+".crt")

	if err := os.WriteFile(keyPath, []byte(keyPEM), 0o440); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, []byte(certPEM), 0o444); err != nil {
		return nil, err
	}
	changes = append(changes, host.Change{Action: host.ActionReplace, Path: certPath, Summary: fmt.Sprintf("rotate %s certificate", spec.Name)})

	return changes, nil
}

func verifyStagedCerts(stageCertDir string, role config.Role) error {
	required := []string{"ca.crt", "kubelet.crt", "kubelet.key", "proxy.crt", "proxy.key"}
	if role == config.RoleMaster {
		required = append(required, "server.crt", "server.key", "admin.crt", "admin.key",
			"controller.crt", "controller.key", "scheduler.crt", "scheduler.key",
			"service_account.key", "service_account_private.key")
	}
	for _, name := range required {
		path := filepath.Join(stageCertDir, name)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("missing %s", name)
		}
		if info.Size() == 0 {
			return fmt.Errorf("empty %s", name)
		}
	}
	return nil
}

func updateAdminKubeconfig(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	certDir := "/etc/kubernetes/certs"
	apiPort := cfg.Shared.KubeAPIPort
	if apiPort == 0 {
		apiPort = 6443
	}

	// Read certs and encode as base64, matching the admin-kubeconfig module format.
	readB64 := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		return base64.StdEncoding.EncodeToString(data)
	}

	content := fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: %s
    server: https://127.0.0.1:%d
  name: %s
contexts:
- context:
    cluster: %s
    user: admin
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: admin
  user:
    client-certificate-data: %s
    client-key-data: %s
`, readB64(certDir+"/ca.crt"), apiPort, cfg.Shared.ClusterUUID,
		cfg.Shared.ClusterUUID,
		readB64(certDir+"/admin.crt"), readB64(certDir+"/admin.key"))

	var changes []host.Change
	change, err := executor.EnsureFile("/etc/kubernetes/admin.conf", []byte(content), 0o600)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}
	// Copy to root kube dir.
	change, err = executor.EnsureCopy("/etc/kubernetes/admin.conf", "/root/.kube/config", 0o600)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}
	return changes, nil
}

func restartServices(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change
	_ = executor.Run("systemctl", "daemon-reload")

	var services []string
	if cfg.Role() == config.RoleMaster {
		services = []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kubelet", "kube-proxy"}
	} else {
		services = []string{"kubelet", "kube-proxy"}
	}

	for _, svc := range services {
		if err := executor.Run("systemctl", "restart", svc); err != nil {
			return nil, fmt.Errorf("restart %s after CA rotation: %w", svc, err)
		}
		changes = append(changes, host.Change{Action: host.ActionRestart, Summary: fmt.Sprintf("restart %s (CA rotation)", svc)})
	}

	return changes, nil
}

func waitForHealthy(cfg config.Config, executor *host.Executor) error {
	if cfg.Role() != config.RoleMaster {
		// Worker: just wait for kubelet to be active.
		for i := 0; i < 30; i++ {
			if executor.SystemctlIsActive("kubelet") {
				return nil
			}
			time.Sleep(2 * time.Second)
		}
		return fmt.Errorf("kubelet did not become active after CA rotation")
	}

	// Master: wait for all control plane services and API health.
	for _, svc := range []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kubelet"} {
		healthy := false
		for i := 0; i < 30; i++ {
			if executor.SystemctlIsActive(svc) {
				healthy = true
				break
			}
			time.Sleep(2 * time.Second)
		}
		if !healthy {
			return fmt.Errorf("service %s did not become active after CA rotation", svc)
		}
	}

	// Check API server health.
	for i := 0; i < 60; i++ {
		err := executor.Run("kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", "get", "--raw=/healthz")
		if err == nil {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("API server health check failed after CA rotation")
}

// computeServiceIP returns the first usable IP in a CIDR (network address + 1).
func computeServiceIP(cidr string) string {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ip = ip.To4()
	if ip == nil {
		return ""
	}
	ip[3]++
	return ip.String()
}

// patchWorkloads annotates all Deployments and DaemonSets to trigger pod
// rollouts so workloads pick up the new CA. Matches bash behavior.
func patchWorkloads(executor *host.Executor, rotationID string) []host.Change {
	var changes []host.Change
	kubeconfig := "/etc/kubernetes/admin.conf"
	annotation := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"magnum.openstack.org/ca-rotation":"%s"}}}}}`, rotationID)

	for _, kind := range []string{"deployment", "daemonset"} {
		// Get all namespaces.
		out, err := executor.RunCapture("kubectl", "--kubeconfig="+kubeconfig,
			"get", "namespaces", "-o", "jsonpath={.items[*].metadata.name}")
		if err != nil {
			continue
		}
		for _, ns := range splitFields(out) {
			resources, err := executor.RunCapture("kubectl", "--kubeconfig="+kubeconfig,
				"get", kind, "-n", ns, "-o", "jsonpath={.items[*].metadata.name}")
			if err != nil || resources == "" {
				continue
			}
			for _, name := range splitFields(resources) {
				_ = executor.Run("kubectl", "--kubeconfig="+kubeconfig,
					"patch", kind, name, "-n", ns, "-p", annotation)
			}
		}
		changes = append(changes, host.Change{
			Action:  host.ActionUpdate,
			Summary: fmt.Sprintf("patch %ss with ca-rotation annotation", kind),
		})
	}
	return changes
}

func splitFields(s string) []string {
	var result []string
	for _, f := range []byte(s) {
		if f == ' ' || f == '\n' || f == '\t' {
			continue
		}
	}
	// Use simple space splitting.
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ' ' || s[i] == '\n' || s[i] == '\t' {
			if i > start {
				result = append(result, s[start:i])
			}
			start = i + 1
		}
	}
	return result
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:CARotation", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"caRotationId": pulumi.String(cfg.Trigger.CARotationID),
		"role":         pulumi.String(cfg.Role().String()),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

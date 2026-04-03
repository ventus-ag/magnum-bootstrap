package mastercerts

import (
	"context"
	"fmt"
	"net"
	"os"

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

func (Module) PhaseID() string { return "master-certificates" }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if cfg.Shared.TLSDisabled {
		return moduleapi.Result{}, nil
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	certDir := "/etc/kubernetes/certs"
	change, err := executor.EnsureDir(certDir, 0o550)
	if err != nil {
		return moduleapi.Result{}, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Build SANs list.
	sans := buildMasterSANs(cfg)

	// Certificate specs for master node.
	specs := []magnumapi.CertSpec{
		{
			Name: "server", CN: "kubernetes",
			SANIPs: sans.ips, SANDNSs: sans.dns,
		},
		{
			Name: "kubelet", CN: fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName),
			O: "system:nodes", SANIPs: sans.ips, SANDNSs: sans.dns,
		},
		{
			Name: "admin", CN: "admin", O: "system:masters",
		},
		{
			Name: "proxy", CN: "system:kube-proxy", O: "system:node-proxier",
		},
		{
			Name: "controller", CN: "system:kube-controller-manager", O: "system:kube-controller-manager",
		},
		{
			Name: "scheduler", CN: "system:kube-scheduler", O: "system:kube-scheduler",
		},
	}

	if !req.Apply {
		// Dry-run: report which certs would be generated.
		for _, spec := range specs {
			if !certExists(certDir, spec.Name) {
				changes = append(changes, host.Change{
					Action:  host.ActionCreate,
					Path:    fmt.Sprintf("%s/%s.crt", certDir, spec.Name),
					Summary: fmt.Sprintf("generate %s certificate", spec.Name),
				})
			}
		}
		return moduleapi.Result{Changes: changes}, nil
	}

	// Create Magnum client.
	client := magnumapi.NewClient(
		cfg.Shared.AuthURL, cfg.Shared.MagnumURL,
		cfg.Shared.TrusteeUserID, cfg.Shared.TrusteePassword,
		cfg.Shared.TrustID, cfg.Shared.ClusterUUID,
		cfg.Shared.VerifyCA,
	)

	// Get Keystone token.
	token, err := client.GetToken()
	if err != nil {
		return moduleapi.Result{}, fmt.Errorf("master-certificates: %w", err)
	}

	// Fetch CA cert if not present.
	caCertPath := certDir + "/ca.crt"
	if _, statErr := os.Stat(caCertPath); os.IsNotExist(statErr) {
		caPEM, err := client.FetchCACert(token)
		if err != nil {
			return moduleapi.Result{}, fmt.Errorf("master-certificates: %w", err)
		}
		change, err := executor.EnsureFile(caCertPath, []byte(caPEM), 0o444)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	// Generate and sign each certificate.
	for _, spec := range specs {
		if certExists(certDir, spec.Name) {
			continue
		}

		keyPEM, csrPEM, err := magnumapi.GenerateKeyAndCSR(spec)
		if err != nil {
			return moduleapi.Result{}, fmt.Errorf("generate %s key/CSR: %w", spec.Name, err)
		}

		certPEM, err := client.SignCSR(token, csrPEM)
		if err != nil {
			return moduleapi.Result{}, fmt.Errorf("sign %s CSR: %w", spec.Name, err)
		}

		keyPath := fmt.Sprintf("%s/%s.key", certDir, spec.Name)
		certPath := fmt.Sprintf("%s/%s.crt", certDir, spec.Name)

		change, err := executor.EnsureFile(keyPath, []byte(keyPEM), 0o440)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}

		change, err = executor.EnsureFile(certPath, []byte(certPEM), 0o444)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	// Write service account keys.
	if cfg.Shared.KubeServiceAccountKey != "" {
		change, err := executor.EnsureFile(certDir+"/service_account.key", []byte(cfg.Shared.KubeServiceAccountKey+"\n"), 0o440)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}
	if cfg.Shared.KubeServiceAccountPrivateKey != "" {
		change, err := executor.EnsureFile(certDir+"/service_account_private.key", []byte(cfg.Shared.KubeServiceAccountPrivateKey+"\n"), 0o440)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	// Create etcd and kube users/groups and set permissions.
	_ = executor.Run("useradd", "-s", "/sbin/nologin", "--system", "etcd")
	_ = executor.Run("useradd", "-s", "/sbin/nologin", "--system", "kube")
	_ = executor.Run("groupadd", "-f", "kube_etcd")
	_ = executor.Run("usermod", "-a", "-G", "kube_etcd", "etcd")
	_ = executor.Run("usermod", "-a", "-G", "kube_etcd", "kube")
	_ = executor.Run("chown", "-R", "kube:kube_etcd", certDir)

	// Copy certs to etcd certs directory.
	etcdCertDir := "/etc/etcd/certs"
	change, err = executor.EnsureDir(etcdCertDir, 0o550)
	if err != nil {
		return moduleapi.Result{}, err
	}
	if change != nil {
		changes = append(changes, *change)
	}
	_ = executor.Run("cp", "-a", certDir+"/.", etcdCertDir+"/")

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"certDir": certDir},
	}, nil
}

type sanSet struct {
	ips []string
	dns []string
}

func buildMasterSANs(cfg config.Config) sanSet {
	var ips []string
	var dns []string

	// Use ResolveNodeIP which falls back to metadata service.
	nodeIP := cfg.ResolveNodeIP()
	if nodeIP != "" {
		ips = append(ips, nodeIP)
	}
	if cfg.Shared.KubeNodePublicIP != "" && cfg.Shared.KubeNodePublicIP != nodeIP {
		ips = append(ips, cfg.Shared.KubeNodePublicIP)
	}
	if cfg.Master != nil {
		if cfg.Master.KubeAPIPublicAddress != "" && !contains(ips, cfg.Master.KubeAPIPublicAddress) {
			ips = append(ips, cfg.Master.KubeAPIPublicAddress)
		}
		if cfg.Master.KubeAPIPrivateAddress != "" && !contains(ips, cfg.Master.KubeAPIPrivateAddress) {
			ips = append(ips, cfg.Master.KubeAPIPrivateAddress)
		}
		if cfg.Master.MasterHostname != "" {
			dns = append(dns, cfg.Master.MasterHostname)
		}
		if cfg.Master.EtcdLBVIP != "" {
			ips = append(ips, cfg.Master.EtcdLBVIP)
		}
	}

	ips = append(ips, "127.0.0.1")

	// Kubernetes service IP (first IP in portal CIDR + 1).
	if cfg.Shared.PortalNetworkCIDR != "" {
		if serviceIP := computeServiceIP(cfg.Shared.PortalNetworkCIDR); serviceIP != "" {
			ips = append(ips, serviceIP)
		}
	}

	dns = append(dns, "kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local")

	return sanSet{ips: ips, dns: dns}
}

func computeServiceIP(cidr string) string {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ip = ip.To4()
	if ip == nil {
		return ""
	}
	// First usable IP: network address + 1.
	ip[3]++
	return ip.String()
}

func certExists(certDir, name string) bool {
	certPath := fmt.Sprintf("%s/%s.crt", certDir, name)
	keyPath := fmt.Sprintf("%s/%s.key", certDir, name)
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	return certErr == nil && keyErr == nil
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:MasterCertificates", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"tlsDisabled": pulumi.Bool(cfg.Shared.TLSDisabled),
		"clusterUuid": pulumi.String(cfg.Shared.ClusterUUID),
		"kubeTag":     pulumi.String(cfg.Shared.KubeTag),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

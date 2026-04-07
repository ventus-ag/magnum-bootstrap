package workercerts

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/certutil"
	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	magnumapi "github.com/ventus-ag/magnum-bootstrap/internal/magnum"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "worker-certificates" }
func (Module) Dependencies() []string { return []string{"prereq-validation"} }

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

	// Build SANs for worker node — fall back to metadata service.
	nodeIP := cfg.ResolveNodeIP()
	var sanIPs []string
	if nodeIP != "" {
		sanIPs = append(sanIPs, nodeIP)
	}
	var sanDNSs []string
	sanDNSs = append(sanDNSs, cfg.Shared.InstanceName)
	if hostname, err := os.Hostname(); err == nil && hostname != cfg.Shared.InstanceName {
		sanDNSs = append(sanDNSs, hostname)
	}

	specs := []magnumapi.CertSpec{
		{
			Name: "kubelet", CN: fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName),
			O: "system:nodes", SANIPs: sanIPs, SANDNSs: sanDNSs,
			KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{
				x509.ExtKeyUsageClientAuth,
				x509.ExtKeyUsageServerAuth,
			},
		},
		{
			Name: "proxy", CN: "system:kube-proxy", O: "system:node-proxier",
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		},
	}

	caCertPath := certDir + "/ca.crt"
	caNeedsRefresh, _ := certutil.CertFileNeedsRefresh(caCertPath)

	if !req.Apply {
		if caNeedsRefresh {
			changes = append(changes, plannedFileChange(caCertPath, "reconcile cluster CA certificate"))
		}
		for _, spec := range specs {
			if needsReconcile, _ := certNeedsReconcile(certDir, spec); needsReconcile {
				changes = append(changes, plannedFileChange(
					fmt.Sprintf("%s/%s.crt", certDir, spec.Name),
					fmt.Sprintf("reconcile %s certificate", spec.Name),
				))
			}
		}
		return moduleapi.Result{Changes: changes}, nil
	}

	needsSigning := caNeedsRefresh
	for _, spec := range specs {
		if needsReconcile, _ := certNeedsReconcile(certDir, spec); needsReconcile {
			needsSigning = true
			break
		}
	}

	var client *magnumapi.Client
	token := ""
	if needsSigning {
		client = magnumapi.NewClient(
			cfg.Shared.AuthURL, cfg.Shared.MagnumURL,
			cfg.Shared.TrusteeUserID, cfg.Shared.TrusteePassword,
			cfg.Shared.TrustID, cfg.Shared.ClusterUUID,
			cfg.Shared.VerifyCA,
		)

		token, err = client.GetToken()
		if err != nil {
			return moduleapi.Result{}, fmt.Errorf("worker-certificates: %w", err)
		}
	}

	// Fetch CA cert if missing or unhealthy.
	if caNeedsRefresh {
		caPEM, err := client.FetchCACert(token)
		if err != nil {
			return moduleapi.Result{}, fmt.Errorf("worker-certificates: %w", err)
		}
		change, err = executor.EnsureFile(caCertPath, []byte(caPEM), 0o444)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	// Generate and sign each certificate if missing or invalid.
	for _, spec := range specs {
		needsReconcile, _ := certNeedsReconcile(certDir, spec)
		if !needsReconcile {
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

	// Set permissions — chmod MUST succeed, kubelet cannot read certs without correct perms.
	if err := executor.Run("chmod", "550", certDir); err != nil {
		return moduleapi.Result{}, fmt.Errorf("worker-certificates: chmod cert dir: %w", err)
	}
	if err := executor.Run("chmod", "440", certDir+"/kubelet.key"); err != nil {
		return moduleapi.Result{}, fmt.Errorf("worker-certificates: chmod kubelet key: %w", err)
	}
	if err := executor.Run("chmod", "440", certDir+"/proxy.key"); err != nil {
		return moduleapi.Result{}, fmt.Errorf("worker-certificates: chmod proxy key: %w", err)
	}

	// Kubelet and kube-proxy must reload certificate changes on workers.
	if len(changes) > 0 && req.Restarts != nil {
		for _, svc := range []string{"kubelet", "kube-proxy"} {
			req.Restarts.Add(svc, "certificate material changed")
		}
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"certDir": certDir},
	}, nil
}

func certNeedsReconcile(certDir string, spec magnumapi.CertSpec) (bool, string) {
	desired := certutil.Spec{
		CommonName:  spec.CN,
		DNSNames:    spec.SANDNSs,
		IPAddresses: parseSANIPs(spec.SANIPs),
		KeyUsage:    spec.KeyUsage,
		ExtKeyUsage: spec.ExtKeyUsage,
	}
	if spec.O != "" {
		desired.Organizations = []string{spec.O}
	}
	return certutil.NeedsReconcile(
		fmt.Sprintf("%s/%s.crt", certDir, spec.Name),
		fmt.Sprintf("%s/%s.key", certDir, spec.Name),
		desired,
	)
}

func plannedFileChange(path, summary string) host.Change {
	action := host.ActionReplace
	if _, err := os.Stat(path); os.IsNotExist(err) {
		action = host.ActionCreate
	}
	return host.Change{Action: action, Path: path, Summary: summary}
}

func parseSANIPs(values []string) []net.IP {
	ips := make([]net.IP, 0, len(values))
	for _, value := range values {
		if ip := net.ParseIP(value); ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips
}

// Destroy removes all certificate files from the kubernetes cert directory.
func (Module) Destroy(_ context.Context, _ config.Config, req moduleapi.Request) error {
	if req.Logger != nil {
		req.Logger.Infof("worker-certificates destroy: removing all files in /etc/kubernetes/certs/")
	}
	_ = os.RemoveAll("/etc/kubernetes/certs/")

	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:WorkerCertificates", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"tlsDisabled": pulumi.Bool(cfg.Shared.TLSDisabled),
		"clusterUuid": pulumi.String(cfg.Shared.ClusterUUID),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

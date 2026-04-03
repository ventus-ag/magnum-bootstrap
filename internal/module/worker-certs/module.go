package workercerts

import (
	"context"
	"fmt"
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

func (Module) PhaseID() string { return "worker-certificates" }

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
		},
		{
			Name: "proxy", CN: "system:kube-proxy", O: "system:node-proxier",
		},
	}

	if !req.Apply {
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

	client := magnumapi.NewClient(
		cfg.Shared.AuthURL, cfg.Shared.MagnumURL,
		cfg.Shared.TrusteeUserID, cfg.Shared.TrusteePassword,
		cfg.Shared.TrustID, cfg.Shared.ClusterUUID,
		cfg.Shared.VerifyCA,
	)

	token, err := client.GetToken()
	if err != nil {
		return moduleapi.Result{}, fmt.Errorf("worker-certificates: %w", err)
	}

	// Fetch CA cert only if not present.
	caCertPath := certDir + "/ca.crt"
	if _, statErr := os.Stat(caCertPath); os.IsNotExist(statErr) {
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

	// Generate and sign each certificate — skip if already exists.
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

	// Set permissions.
	_ = executor.Run("chmod", "550", certDir)
	_ = executor.Run("chmod", "440", certDir+"/kubelet.key")
	_ = executor.Run("chmod", "440", certDir+"/proxy.key")

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"certDir": certDir},
	}, nil
}

func certExists(certDir, name string) bool {
	certPath := fmt.Sprintf("%s/%s.crt", certDir, name)
	keyPath := fmt.Sprintf("%s/%s.key", certDir, name)
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	return certErr == nil && keyErr == nil
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

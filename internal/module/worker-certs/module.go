package workercerts

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/certutil"
	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	magnumapi "github.com/ventus-ag/magnum-bootstrap/internal/magnum"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "worker-certificates" }
func (Module) Dependencies() []string { return []string{"ca-rotation"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if cfg.Shared.TLSDisabled {
		return moduleapi.Result{}, nil
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	certDir := "/etc/kubernetes/certs"
	dirResult, err := (hostresource.DirectorySpec{Path: certDir, Mode: 0o550}).Apply(executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, dirResult.Changes...)

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
		fileResult, err := (hostresource.FileSpec{Path: caCertPath, Content: []byte(caPEM), Mode: 0o444}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, fileResult.Changes...)
	}

	// Generate and sign certificates in parallel, then write files in
	// deterministic spec order.
	signSpecs := make([]magnumapi.CertSpec, 0, len(specs))
	for _, spec := range specs {
		needsReconcile, _ := certNeedsReconcile(certDir, spec)
		if !needsReconcile {
			continue
		}
		signSpecs = append(signSpecs, spec)
	}

	signedCerts, err := magnumapi.GenerateAndSignCerts(client, token, signSpecs)
	if err != nil {
		return moduleapi.Result{}, err
	}
	for _, signed := range signedCerts {
		spec := signed.Spec
		keyPath := fmt.Sprintf("%s/%s.key", certDir, spec.Name)
		certPath := fmt.Sprintf("%s/%s.crt", certDir, spec.Name)

		keyResult, err := (hostresource.FileSpec{Path: keyPath, Content: []byte(signed.KeyPEM), Mode: 0o440}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, keyResult.Changes...)

		certResult, err := (hostresource.FileSpec{Path: certPath, Content: []byte(signed.CertPEM), Mode: 0o444}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, certResult.Changes...)
	}

	// Set permissions — chmod MUST succeed, kubelet cannot read certs without correct perms.
	for _, modeSpec := range []hostresource.ModeSpec{
		{Path: certDir, Mode: 0o550},
		{Path: certDir + "/kubelet.key", Mode: 0o440, SkipIfMissing: true},
		{Path: certDir + "/proxy.key", Mode: 0o440, SkipIfMissing: true},
	} {
		result, err := modeSpec.Apply(executor)
		if err != nil {
			return moduleapi.Result{}, fmt.Errorf("worker-certificates: apply mode on %s: %w", modeSpec.Path, err)
		}
		changes = append(changes, result.Changes...)
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
	if !cfg.Shared.TLSDisabled {
		childOpts := hostresource.ChildResourceOptions(res, opts...)
		certDirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-cert-dir", hostresource.DirectorySpec{Path: "/etc/kubernetes/certs", Mode: 0o550}, childOpts...)
		if err != nil {
			return nil, err
		}
		fileDeps := []pulumi.Resource{certDirRes}
		for _, path := range []string{"/etc/kubernetes/certs/ca.crt", "/etc/kubernetes/certs/kubelet.key", "/etc/kubernetes/certs/kubelet.crt", "/etc/kubernetes/certs/proxy.key", "/etc/kubernetes/certs/proxy.crt"} {
			if data, err := os.ReadFile(path); err == nil {
				mode := os.FileMode(0o444)
				if strings.HasSuffix(path, ".key") {
					mode = 0o440
				}
				fileOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, certDirRes)
				fileRes, err := hostsdk.RegisterFileSpec(ctx, name+"-"+strings.ReplaceAll(strings.Trim(path, "/"), "/", "-"), hostresource.FileSpec{Path: path, Content: data, Mode: mode}, fileOpts...)
				if err != nil {
					return nil, err
				}
				fileDeps = append(fileDeps, fileRes)
			}
		}
		for _, modeSpec := range []hostresource.ModeSpec{
			{Path: "/etc/kubernetes/certs", Mode: 0o550},
			{Path: "/etc/kubernetes/certs/kubelet.key", Mode: 0o440, SkipIfMissing: true},
			{Path: "/etc/kubernetes/certs/proxy.key", Mode: 0o440, SkipIfMissing: true},
		} {
			modeOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, fileDeps...)
			if _, err := hostsdk.RegisterModeSpec(ctx, name+"-mode-"+strings.ReplaceAll(strings.Trim(modeSpec.Path, "/"), "/", "-"), modeSpec, modeOpts...); err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"tlsDisabled": pulumi.Bool(cfg.Shared.TLSDisabled),
		"clusterUuid": pulumi.String(cfg.Shared.ClusterUUID),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

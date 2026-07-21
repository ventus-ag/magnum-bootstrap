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
		{
			// Scoped identity for node metadata (labels/taints) reconciliation.
			// Deliberately NOT in system:nodes so NodeRestriction does not
			// apply; authorized only by the magnum:node-manager ClusterRole
			// that cluster-rbac binds to this group.
			Name: "node-manager", CN: fmt.Sprintf("magnum:node-manager:%s", cfg.Shared.InstanceName),
			O:           "magnum:node-manager",
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		},
	}

	caCertPath := certDir + "/ca.crt"
	caNeedsRefresh, _ := certutil.CertFileNeedsRefresh(caCertPath)

	// Auto-heal "changed by client": when any live leaf no longer chains to the
	// on-disk CA (the CA or a leaf was replaced out from under us), re-fetch the
	// canonical CA from Barbican and re-sign every affected leaf against it.
	// Gated off only during an ACTIVE dual-CA rotation. Uses Operation()
	// (applied-aware), NOT IsPureCARotation() — the latter stays true forever
	// once CA_ROTATION_ID lingers in heat-params after a completed rotation.
	checkChain := cfg.Operation() != config.OperationCARotate
	if checkChain && !caNeedsRefresh {
		for _, spec := range specs {
			if certutil.LeafChainBroken(fmt.Sprintf("%s/%s.crt", certDir, spec.Name), caCertPath) {
				caNeedsRefresh = true
				break
			}
		}
	}

	if !req.Apply {
		if caNeedsRefresh {
			changes = append(changes, plannedFileChange(caCertPath, "reconcile cluster CA certificate"))
		}
		for _, spec := range specs {
			if needsReconcile, _ := certNeedsReconcile(certDir, spec, checkChain); needsReconcile {
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
		if needsReconcile, _ := certNeedsReconcile(certDir, spec, checkChain); needsReconcile {
			needsSigning = true
			break
		}
	}

	// Locally-restored CA fallback (mirrors master-certificates): a worker only
	// has a CA private key when an operator hand-restored the cluster and left
	// ca.key beside ca.crt. When that pair is valid but Barbican's CA is
	// unusable, mismatched, or unreachable, keep converging on the local CA and
	// sign leaf certs locally instead of wedging — the operator's later
	// ca-rotate reconverges the cluster on the canonical Barbican CA. Workers
	// without a local ca.key keep the existing fail-fast behavior.
	caKeyPath := certDir + "/ca.key"
	signLocally := false
	var warnings []string
	fallBackToLocalCA := func(reason string) bool {
		if ok, why := certutil.LocalCAUsable(caCertPath, caKeyPath); !ok {
			// The certs-dir pair is unusable, but a hand-rotated CA may live in
			// the live kubeconfigs instead. Adopt it when it pairs with ca.key.
			recovered := false
			for _, kubeconfigPath := range []string{
				"/etc/kubernetes/admin.conf",
				"/etc/kubernetes/kubelet.conf",
				"/etc/kubernetes/kubelet-config.yaml", // legacy bash-era name
			} {
				kcCA, kcErr := certutil.CAFromKubeconfig(kubeconfigPath)
				if kcErr != nil {
					continue
				}
				if bad, _ := certutil.CertPEMNeedsRefresh(kcCA); bad {
					continue
				}
				if !certutil.CertPEMMatchesKeyFile(kcCA, caKeyPath) {
					continue
				}
				fileResult, applyErr := (hostresource.FileSpec{Path: caCertPath, Content: kcCA, Mode: 0o444}).Apply(executor)
				if applyErr != nil {
					req.Logger.Warnf("worker-certificates: cannot adopt hand-rotated CA from %s: %v", kubeconfigPath, applyErr)
					return false
				}
				changes = append(changes, fileResult.Changes...)
				req.Logger.Warnf("worker-certificates: adopted hand-rotated CA from %s into %s (it pairs with ca.key)", kubeconfigPath, caCertPath)
				recovered = true
				break
			}
			if !recovered {
				// Last resort (workers carrying a hand-restored ca.key only):
				// re-date the cluster's own expired CA from its key — same
				// public key, same subject, fresh validity. Refuses to touch a
				// non-expired CA or a cert/key non-pair; workers without a
				// local ca.key always land in the fail-fast below.
				renewedPEM, renewErr := certutil.RenewExpiredCA(caCertPath, caKeyPath)
				if renewErr == nil {
					fileResult, applyErr := (hostresource.FileSpec{Path: caCertPath, Content: renewedPEM, Mode: 0o444}).Apply(executor)
					if applyErr != nil {
						req.Logger.Warnf("worker-certificates: cannot install renewed CA: %v", applyErr)
						return false
					}
					changes = append(changes, fileResult.Changes...)
					req.Logger.Warnf("worker-certificates: cluster CA was expired — renewed it from its own key (same public key, fresh validity)")
					recovered = true
				} else {
					req.Logger.Infof("worker-certificates: expired-CA renewal unavailable (%v)", renewErr)
				}
			}
			if !recovered {
				req.Logger.Infof("worker-certificates: local CA fallback unavailable (%s)", why)
				return false
			}
		}
		msg := fmt.Sprintf("%s; the local ca.crt/ca.key pair is valid — continuing on the locally-restored CA and signing leaf certificates locally. Trigger a CA rotation (ca-rotate) to reconverge on the canonical Barbican CA", reason)
		req.Logger.Warnf("worker-certificates: %s", msg)
		warnings = append(warnings, msg)
		signLocally = true
		caNeedsRefresh = false
		return true
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
		if err != nil && !fallBackToLocalCA(fmt.Sprintf("cannot authenticate to OpenStack for certificate signing (%v)", err)) {
			return moduleapi.Result{}, fmt.Errorf("worker-certificates: %w", err)
		}
	}

	// Fetch CA cert if missing or unhealthy.
	if caNeedsRefresh {
		caPEM, err := client.FetchCACert(token)
		if err != nil {
			if !fallBackToLocalCA(fmt.Sprintf("cannot fetch the CA certificate from Barbican (%v)", err)) {
				return moduleapi.Result{}, fmt.Errorf("worker-certificates: %w", err)
			}
		} else if expired, why := certutil.CertPEMNeedsRefresh([]byte(caPEM)); expired {
			// Recovery guard (normal reconcile only): if Barbican still serves
			// an unusable CA, re-signing leaves against it is futile and the
			// doomed kubelet restart burns the Heat window. Fall back to a
			// valid local CA pair when one exists; otherwise fail fast with an
			// actionable message so the operator triggers a CA rotation.
			if !fallBackToLocalCA(fmt.Sprintf("Barbican serves an unusable CA cert (%s)", why)) {
				msg := fmt.Sprintf("Barbican serves an unusable CA cert (%s); this node cannot self-recover on a normal reconcile — trigger a CA rotation (ca-rotate) to mint a new CA", why)
				req.Logger.Warnf("worker-certificates: %s", msg)
				return moduleapi.Result{Warnings: []string{msg}}, fmt.Errorf("worker-certificates: %s", msg)
			}
		} else if _, statErr := os.Stat(caKeyPath); statErr == nil && !certutil.CertPEMMatchesKeyFile([]byte(caPEM), caKeyPath) {
			// A worker carrying a hand-restored ca.key: installing a Barbican CA
			// that does not pair with it would flip this node's trust away from
			// the CA the (equally hand-restored) masters run on.
			if !fallBackToLocalCA("Barbican's CA does not match the locally-restored CA signing key (ca.key)") {
				msg := "Barbican CA does not match the locally-restored ca.key on this node; installing it would split node trust — preserving the working material. Trigger a CA rotation (ca-rotate) to converge"
				req.Logger.Warnf("worker-certificates: %s", msg)
				return moduleapi.Result{Warnings: []string{msg}}, fmt.Errorf("worker-certificates: %s", msg)
			}
		} else {
			fileResult, err := (hostresource.FileSpec{Path: caCertPath, Content: []byte(caPEM), Mode: 0o444}).Apply(executor)
			if err != nil {
				return moduleapi.Result{}, err
			}
			changes = append(changes, fileResult.Changes...)
		}
	}

	// Generate and sign certificates in parallel, then write files in
	// deterministic spec order.
	signSpecs := make([]magnumapi.CertSpec, 0, len(specs))
	for _, spec := range specs {
		needsReconcile, _ := certNeedsReconcile(certDir, spec, checkChain)
		if !needsReconcile {
			continue
		}
		signSpecs = append(signSpecs, spec)
	}

	var signedCerts []magnumapi.SignedCert
	if signLocally {
		signedCerts, err = magnumapi.GenerateLocalSignedCerts(caCertPath, caKeyPath, signSpecs)
	} else {
		signedCerts, err = magnumapi.GenerateAndSignCerts(client, token, signSpecs)
		// Vet API-signed material against the on-disk CA before installing it
		// (see master-certificates for the rationale — a Barbican/local CA
		// mismatch is only visible here when no CA refetch happened this run).
		if err == nil && anyLeafPEMChainBroken(signedCerts, caCertPath) {
			if fallBackToLocalCA("Magnum-signed certificates do not chain to the on-disk CA (Barbican CA differs from the local CA)") {
				signedCerts, err = magnumapi.GenerateLocalSignedCerts(caCertPath, caKeyPath, signSpecs)
			} else {
				msg := "Magnum-signed certificates do not chain to the on-disk CA and no usable local CA pair exists — trigger a CA rotation (ca-rotate) to converge"
				req.Logger.Warnf("worker-certificates: %s", msg)
				return moduleapi.Result{Warnings: []string{msg}}, fmt.Errorf("worker-certificates: %s", msg)
			}
		}
	}
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
		{Path: certDir + "/node-manager.key", Mode: 0o440, SkipIfMissing: true},
	} {
		result, err := modeSpec.Apply(executor)
		if err != nil {
			return moduleapi.Result{}, fmt.Errorf("worker-certificates: apply mode on %s: %w", modeSpec.Path, err)
		}
		changes = append(changes, result.Changes...)
	}

	// Kubelet and kube-proxy must reload certificate changes on workers.
	// The node-manager cert is used only by the reconciler itself — its
	// (re)issuance alone must not bounce node services.
	restartRelevant := false
	for _, change := range changes {
		if !strings.Contains(change.Summary, "node-manager") && !strings.Contains(change.Path, "node-manager") {
			restartRelevant = true
			break
		}
	}
	if restartRelevant && req.Restarts != nil {
		for _, svc := range []string{"kubelet", "kube-proxy"} {
			req.Restarts.Add(svc, "certificate material changed")
		}
	}

	return moduleapi.Result{
		Changes:  changes,
		Warnings: warnings,
		Outputs:  map[string]string{"certDir": certDir},
	}, nil
}

func certNeedsReconcile(certDir string, spec magnumapi.CertSpec, checkChain bool) (bool, string) {
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
	certPath := fmt.Sprintf("%s/%s.crt", certDir, spec.Name)
	if needs, reason := certutil.NeedsReconcile(certPath, fmt.Sprintf("%s/%s.key", certDir, spec.Name), desired); needs {
		return true, reason
	}
	// The cert matches its spec and is valid, but may no longer chain to the
	// current on-disk CA (CA or leaf replaced by the client). Re-sign it so it
	// chains to the CA the rest of the control plane trusts.
	if checkChain && certutil.LeafChainBroken(certPath, certDir+"/ca.crt") {
		return true, "leaf no longer chains to CA"
	}
	return false, ""
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
		for _, path := range []string{"/etc/kubernetes/certs/ca.crt", "/etc/kubernetes/certs/kubelet.key", "/etc/kubernetes/certs/kubelet.crt", "/etc/kubernetes/certs/proxy.key", "/etc/kubernetes/certs/proxy.crt", "/etc/kubernetes/certs/node-manager.key", "/etc/kubernetes/certs/node-manager.crt"} {
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
			{Path: "/etc/kubernetes/certs/node-manager.key", Mode: 0o440, SkipIfMissing: true},
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

// anyLeafPEMChainBroken reports whether any freshly signed certificate fails to
// chain to the on-disk CA. Empty input reports false.
func anyLeafPEMChainBroken(signed []magnumapi.SignedCert, caPath string) bool {
	for _, sc := range signed {
		if !certutil.LeafPEMSignedByCAFile([]byte(sc.CertPEM), caPath) {
			return true
		}
	}
	return false
}

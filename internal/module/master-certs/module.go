package mastercerts

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
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

func (Module) PhaseID() string        { return "master-certificates" }
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

	// Build SANs list.
	sans := buildMasterSANs(cfg)

	// Certificate specs for master node.
	specs := []magnumapi.CertSpec{
		{
			Name: "server", CN: "kubernetes",
			SANIPs: sans.ips, SANDNSs: sans.dns,
			ExtKeyUsage: []x509.ExtKeyUsage{
				x509.ExtKeyUsageClientAuth,
				x509.ExtKeyUsageServerAuth,
			},
		},
		{
			Name: "kubelet", CN: fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName),
			O: "system:nodes", SANIPs: sans.ips, SANDNSs: sans.dns,
			KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{
				x509.ExtKeyUsageClientAuth,
				x509.ExtKeyUsageServerAuth,
			},
		},
		{
			Name: "admin", CN: "admin", O: "system:masters",
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		},
		{
			Name: "proxy", CN: "system:kube-proxy", O: "system:node-proxier",
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		},
		{
			Name: "controller", CN: "system:kube-controller-manager", O: "system:kube-controller-manager",
			KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{
				x509.ExtKeyUsageClientAuth,
				x509.ExtKeyUsageServerAuth,
			},
		},
		{
			Name: "scheduler", CN: "system:kube-scheduler", O: "system:kube-scheduler",
			KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{
				x509.ExtKeyUsageClientAuth,
				x509.ExtKeyUsageServerAuth,
			},
		},
	}

	caCertPath := certDir + "/ca.crt"
	caNeedsRefresh, _ := certutil.CertFileNeedsRefresh(caCertPath)

	// Auto-heal "changed by client": when any live leaf no longer chains to the
	// on-disk CA (the CA or a leaf was replaced out from under us — e.g. by the
	// cluster owner), re-fetch the canonical CA from Barbican and re-sign every
	// affected leaf against it. Gated off only during an ACTIVE dual-CA rotation,
	// which owns cert material and where the live ca.crt is intentionally a
	// new+old bundle. Uses Operation() (applied-aware via AppliedCARotationID),
	// NOT IsPureCARotation() — the latter stays true forever once CA_ROTATION_ID
	// lingers in heat-params after a completed rotation, which would permanently
	// disable this heal on every rotated cluster.
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
		// Dry-run: report which certs would be generated or replaced.
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

	// Locally-restored CA fallback: when Barbican's CA is unusable, mismatched,
	// or unreachable but the node carries a valid, matching ca.crt/ca.key pair
	// (typical after an operator hand-restored an expired cluster with a
	// locally-minted CA), keep converging on that local CA and sign leaf certs
	// locally instead of wedging. The cluster stays alive on the "unknown" CA;
	// the operator later triggers a CA rotation (ca-rotate), which mints a new
	// canonical CA in Barbican and carries the local CA in the dual-CA trust
	// bundle as the old side — reconverging everything without downtime.
	caKeyPath := certDir + "/ca.key"
	signLocally := false
	var warnings []string
	fallBackToLocalCA := func(reason string) bool {
		if ok, why := certutil.LocalCAUsable(caCertPath, caKeyPath); !ok {
			// The certs-dir pair is unusable, but a hand-rotated CA may live in
			// the live kubeconfigs instead (operator updated admin.conf and the
			// component kubeconfigs without rewriting certs/ca.crt). Adopt the
			// kubeconfig-embedded CA when it is valid and pairs with ca.key.
			recovered := false
			for _, kubeconfigPath := range []string{"/etc/kubernetes/admin.conf"} {
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
					req.Logger.Warnf("master-certificates: cannot adopt hand-rotated CA from %s: %v", kubeconfigPath, applyErr)
					return false
				}
				changes = append(changes, fileResult.Changes...)
				req.Logger.Warnf("master-certificates: adopted hand-rotated CA from %s into %s (it pairs with ca.key)", kubeconfigPath, caCertPath)
				recovered = true
				break
			}
			if !recovered {
				// Last resort: the cluster's own CA expired before anyone
				// rotated it (Barbican serves the same expired CA, so neither
				// fetch nor adoption can help). Re-date it from its own key:
				// same public key, same subject, fresh validity — every
				// existing leaf still chains, SA keys untouched. Refuses to
				// touch a non-expired CA or a cert/key non-pair.
				renewedPEM, renewErr := certutil.RenewExpiredCA(caCertPath, caKeyPath)
				if renewErr == nil {
					fileResult, applyErr := (hostresource.FileSpec{Path: caCertPath, Content: renewedPEM, Mode: 0o444}).Apply(executor)
					if applyErr != nil {
						req.Logger.Warnf("master-certificates: cannot install renewed CA: %v", applyErr)
						return false
					}
					changes = append(changes, fileResult.Changes...)
					req.Logger.Warnf("master-certificates: cluster CA was expired — renewed it from its own key (same public key, fresh validity)")
					recovered = true
				} else {
					req.Logger.Infof("master-certificates: expired-CA renewal unavailable (%v)", renewErr)
				}
			}
			if !recovered {
				req.Logger.Infof("master-certificates: local CA fallback unavailable (%s)", why)
				return false
			}
		}
		msg := fmt.Sprintf("%s; the local ca.crt/ca.key pair is valid — continuing on the locally-restored CA and signing leaf certificates locally. Trigger a CA rotation (ca-rotate) to reconverge on the canonical Barbican CA", reason)
		req.Logger.Warnf("master-certificates: %s", msg)
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
			return moduleapi.Result{}, fmt.Errorf("master-certificates: %w", err)
		}
	}

	// Fetch CA cert if missing or unhealthy.
	if caNeedsRefresh {
		caPEM, err := client.FetchCACert(token)
		if err != nil {
			if !fallBackToLocalCA(fmt.Sprintf("cannot fetch the CA certificate from Barbican (%v)", err)) {
				return moduleapi.Result{}, fmt.Errorf("master-certificates: %w", err)
			}
		} else {
			// Recovery guards (normal reconcile only — a ca-rotate supplies
			// fresh, mutually-consistent material via the ca-rotation phase and
			// never reaches here with a broken CA). Installing an unusable CA
			// over the live material corrupts a wedged-but-consistent cluster
			// into a split state, and the doomed control-plane restart that
			// follows would burn the whole Heat window. Fall back to a valid
			// local CA pair when one exists; otherwise fail fast at this phase
			// with an actionable message.
			haveCAKey := false
			if _, statErr := os.Stat(caKeyPath); statErr == nil {
				haveCAKey = true
			}
			if expired, why := certutil.CertPEMNeedsRefresh([]byte(caPEM)); expired {
				if !fallBackToLocalCA(fmt.Sprintf("Barbican serves an unusable CA cert (%s)", why)) {
					msg := fmt.Sprintf("Barbican serves an unusable CA cert (%s); this node cannot self-recover on a normal reconcile — trigger a CA rotation (ca-rotate) to mint a new CA", why)
					req.Logger.Warnf("master-certificates: %s", msg)
					return moduleapi.Result{Warnings: []string{msg}}, fmt.Errorf("master-certificates: %s", msg)
				}
			} else if haveCAKey && !certutil.CertPEMMatchesKeyFile([]byte(caPEM), caKeyPath) {
				if !fallBackToLocalCA("Barbican's CA does not match this cluster's CA signing key (ca.key)") {
					msg := "Barbican CA no longer matches this cluster's CA signing key (ca.key); installing it would split the CA cert/key pair and crashloop kube-controller-manager — preserving the working pair. Trigger a CA rotation (ca-rotate) to converge"
					req.Logger.Warnf("master-certificates: %s", msg)
					return moduleapi.Result{Warnings: []string{msg}}, fmt.Errorf("master-certificates: %s", msg)
				}
			} else {
				fileResult, err := (hostresource.FileSpec{Path: caCertPath, Content: []byte(caPEM), Mode: 0o444}).Apply(executor)
				if err != nil {
					return moduleapi.Result{}, err
				}
				changes = append(changes, fileResult.Changes...)
			}
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
		// Vet API-signed material against the on-disk CA before installing it.
		// A hand-restored cluster whose leaves still chain to the local CA never
		// triggers a CA refetch this run, so a Barbican mismatch is only visible
		// here: Magnum signs with ITS CA, and installing those leaves over a
		// node trusting the local CA breaks component auth until the next run.
		if err == nil && anyLeafPEMChainBroken(signedCerts, caCertPath) {
			if fallBackToLocalCA("Magnum-signed certificates do not chain to the on-disk CA (Barbican CA differs from the local CA)") {
				signedCerts, err = magnumapi.GenerateLocalSignedCerts(caCertPath, caKeyPath, signSpecs)
			} else {
				msg := "Magnum-signed certificates do not chain to the on-disk CA and no usable local CA pair exists — trigger a CA rotation (ca-rotate) to converge"
				req.Logger.Warnf("master-certificates: %s", msg)
				return moduleapi.Result{Warnings: []string{msg}}, fmt.Errorf("master-certificates: %s", msg)
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

	// Write service account signing keys — ONLY if absent. The SA keypair
	// (service_account.key = public verify key for kube-apiserver,
	// service_account_private.key = private signing key for
	// kube-controller-manager) is established at cluster creation and must stay
	// IDENTICAL across the cluster's entire life. Overwriting a live key with a
	// stale or different heat-param value invalidates EVERY existing
	// ServiceAccount token cluster-wide (all in-cluster API clients get 401
	// until their pods restart). Once on disk the local key is authoritative; a
	// genuinely fresh node (resize) has none and correctly seeds from
	// heat-params. This is not a key we rotate here, so absent-only is safe for
	// upgrade and CA rotation alike.
	saChanges, err := writeSAKeyIfAbsent(executor, certDir+"/service_account.key", cfg.Shared.KubeServiceAccountKey)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, saChanges...)
	saPrivChanges, err := writeSAKeyIfAbsent(executor, certDir+"/service_account_private.key", cfg.Shared.KubeServiceAccountPrivateKey)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, saPrivChanges...)

	// Create etcd and kube users/groups and set permissions.
	// useradd/groupadd/usermod may fail if the user/group already exists — log
	// the output for debugging but do not treat as fatal.
	if err := executor.Run("useradd", "-s", "/sbin/nologin", "--system", "etcd"); err != nil {
		req.Logger.Infof("master-certificates: useradd etcd (non-fatal): %v", err)
	}
	if err := executor.Run("useradd", "-s", "/sbin/nologin", "--system", "kube"); err != nil {
		req.Logger.Infof("master-certificates: useradd kube (non-fatal): %v", err)
	}
	if err := executor.Run("groupadd", "-f", "kube_etcd"); err != nil {
		req.Logger.Infof("master-certificates: groupadd kube_etcd (non-fatal): %v", err)
	}
	if err := executor.Run("usermod", "-a", "-G", "kube_etcd", "etcd"); err != nil {
		req.Logger.Infof("master-certificates: usermod etcd (non-fatal): %v", err)
	}
	if err := executor.Run("usermod", "-a", "-G", "kube_etcd", "kube"); err != nil {
		req.Logger.Infof("master-certificates: usermod kube (non-fatal): %v", err)
	}
	// chown MUST succeed — wrong permissions will break services.
	ownershipResult, err := (hostresource.OwnershipSpec{Path: certDir, Owner: "kube", Group: "kube_etcd", Recursive: true}).Apply(executor)
	if err != nil {
		return moduleapi.Result{}, fmt.Errorf("master-certificates: set cert dir ownership: %w", err)
	}
	changes = append(changes, ownershipResult.Changes...)

	// Copy certs to etcd certs directory.
	etcdCertDir := "/etc/etcd/certs"
	etcdDirResult, err := (hostresource.DirectorySpec{Path: etcdCertDir, Mode: 0o550}).Apply(executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, etcdDirResult.Changes...)
	for _, fileName := range etcdCopyFiles(cfg) {
		sourcePath := certDir + "/" + fileName
		info, statErr := os.Stat(sourcePath)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return moduleapi.Result{}, fmt.Errorf("master-certificates: stat source cert %s: %w", sourcePath, statErr)
		}
		copyResult, err := (hostresource.CopySpec{Source: sourcePath, Path: etcdCertDir + "/" + fileName, Mode: info.Mode().Perm()}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, fmt.Errorf("master-certificates: copy cert %s to etcd dir: %w", fileName, err)
		}
		changes = append(changes, copyResult.Changes...)
	}
	etcdOwnershipResult, err := (hostresource.OwnershipSpec{Path: etcdCertDir, Owner: "kube", Group: "kube_etcd", Recursive: true}).Apply(executor)
	if err != nil {
		return moduleapi.Result{}, fmt.Errorf("master-certificates: set etcd cert dir ownership: %w", err)
	}
	changes = append(changes, etcdOwnershipResult.Changes...)

	// Any certificate material change requires consumers to reload it.
	if len(changes) > 0 && req.Restarts != nil {
		for _, svc := range []string{
			"etcd",
			"kube-apiserver",
			"kube-controller-manager",
			"kube-scheduler",
			"kubelet",
			"kube-proxy",
		} {
			req.Restarts.Add(svc, "certificate material changed")
		}
	}

	return moduleapi.Result{
		Changes:  changes,
		Outputs:  map[string]string{"certDir": certDir},
		Warnings: warnings,
	}, nil
}

// writeSAKeyIfAbsent writes a service-account signing key from heat-params only
// when the file does not already exist (or is empty). An existing non-empty key
// is the cluster's live, authoritative SA material and is left untouched — see
// the call site for why overwriting it is catastrophic.
func writeSAKeyIfAbsent(executor *host.Executor, path, content string) ([]host.Change, error) {
	if strings.TrimSpace(content) == "" {
		return nil, nil
	}
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		if executor.Logger != nil {
			executor.Logger.Infof("master-certificates: %s present, preserving live service-account key (not overwriting from heat-params)", path)
		}
		return nil, nil
	}
	result, err := (hostresource.FileSpec{Path: path, Content: []byte(content + "\n"), Mode: 0o440}).Apply(executor)
	if err != nil {
		return nil, err
	}
	return result.Changes, nil
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
	// After a dual-CA rotation whose coordinated cutover barrier failed partway
	// (a wedged/absent node stalled the barrier before the leaf-swap stage),
	// ca.key + Barbican already point at the new CA but the leaves were never
	// re-signed: each is still OLD-CA signed yet still chains to the old CA
	// RETAINED in the ca.crt bundle, so the check above passes and it is never
	// fixed. A freshly provisioned node then fetches only the new Barbican CA and
	// cannot verify the old-signed control plane. Re-sign any leaf not signed by
	// the CA that pairs with the current ca.key, completing the skipped cutover on
	// an ordinary reconcile. Safe: that CA is already in the trust bundle and
	// served by Barbican, so every node already trusts it. Masters carry ca.key;
	// on a worker (no ca.key) this is a no-op.
	if checkChain && certutil.LeafNotSignedByCurrentCA(certPath, certDir+"/ca.crt", certDir+"/ca.key") {
		return true, "leaf not signed by current CA (completing a stalled ca-rotate cutover)"
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

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// Destroy removes all certificate files from the kubernetes and etcd cert directories.
func (Module) Destroy(_ context.Context, _ config.Config, req moduleapi.Request) error {
	if req.Logger != nil {
		req.Logger.Infof("master-certificates destroy: removing all files in /etc/kubernetes/certs/ and /etc/etcd/certs/")
	}
	_ = os.RemoveAll("/etc/kubernetes/certs/")
	_ = os.RemoveAll("/etc/etcd/certs/")

	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:MasterCertificates", name, res, opts...); err != nil {
		return nil, err
	}
	if !cfg.Shared.TLSDisabled {
		childOpts := hostresource.ChildResourceOptions(res, opts...)
		kubeDirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-kube-cert-dir", hostresource.DirectorySpec{Path: "/etc/kubernetes/certs", Mode: 0o550}, childOpts...)
		if err != nil {
			return nil, err
		}
		etcdDirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-etcd-cert-dir", hostresource.DirectorySpec{Path: "/etc/etcd/certs", Mode: 0o550}, childOpts...)
		if err != nil {
			return nil, err
		}
		kubeFileResources := []pulumi.Resource{kubeDirRes}
		sourceResources := map[string]pulumi.Resource{}
		for _, path := range []string{
			"/etc/kubernetes/certs/ca.crt",
			"/etc/kubernetes/certs/server.key",
			"/etc/kubernetes/certs/server.crt",
			"/etc/kubernetes/certs/kubelet.key",
			"/etc/kubernetes/certs/kubelet.crt",
			"/etc/kubernetes/certs/admin.key",
			"/etc/kubernetes/certs/admin.crt",
			"/etc/kubernetes/certs/proxy.key",
			"/etc/kubernetes/certs/proxy.crt",
			"/etc/kubernetes/certs/controller.key",
			"/etc/kubernetes/certs/controller.crt",
			"/etc/kubernetes/certs/scheduler.key",
			"/etc/kubernetes/certs/scheduler.crt",
			"/etc/kubernetes/certs/service_account.key",
			"/etc/kubernetes/certs/service_account_private.key",
		} {
			if data, err := os.ReadFile(path); err == nil {
				mode := os.FileMode(0o444)
				if strings.HasSuffix(path, ".key") {
					mode = 0o440
				}
				fileOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, kubeDirRes)
				fileRes, err := hostsdk.RegisterFileSpec(ctx, name+"-"+strings.ReplaceAll(strings.Trim(path, "/"), "/", "-"), hostresource.FileSpec{Path: path, Content: data, Mode: mode}, fileOpts...)
				if err != nil {
					return nil, err
				}
				kubeFileResources = append(kubeFileResources, fileRes)
				sourceResources[filepath.Base(path)] = fileRes
			}
		}
		ownershipOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, kubeFileResources...)
		if _, err := hostsdk.RegisterOwnershipSpec(ctx, name+"-kube-cert-ownership", hostresource.OwnershipSpec{Path: "/etc/kubernetes/certs", Owner: "kube", Group: "kube_etcd", Recursive: true}, ownershipOpts...); err != nil {
			return nil, err
		}
		etcdCopyResources := []pulumi.Resource{etcdDirRes}
		for _, fileName := range etcdCopyFiles(cfg) {
			sourcePath := "/etc/kubernetes/certs/" + fileName
			mode := os.FileMode(0o444)
			if strings.HasSuffix(fileName, ".key") {
				mode = 0o440
			}
			deps := []pulumi.Resource{etcdDirRes}
			if sourceRes, ok := sourceResources[fileName]; ok {
				deps = append(deps, sourceRes)
			}
			copyOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, deps...)
			copyRes, err := hostsdk.RegisterCopySpec(ctx, name+"-etcd-copy-"+strings.ReplaceAll(fileName, ".", "-"), hostresource.CopySpec{Source: sourcePath, Path: "/etc/etcd/certs/" + fileName, Mode: mode}, copyOpts...)
			if err != nil {
				return nil, err
			}
			etcdCopyResources = append(etcdCopyResources, copyRes)
		}
		etcdOwnershipOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, etcdCopyResources...)
		if _, err := hostsdk.RegisterOwnershipSpec(ctx, name+"-etcd-cert-ownership", hostresource.OwnershipSpec{Path: "/etc/etcd/certs", Owner: "kube", Group: "kube_etcd", Recursive: true}, etcdOwnershipOpts...); err != nil {
			return nil, err
		}
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

func etcdCopyFiles(cfg config.Config) []string {
	files := []string{
		"ca.crt",
		"server.key",
		"server.crt",
		"kubelet.key",
		"kubelet.crt",
		"admin.key",
		"admin.crt",
		"proxy.key",
		"proxy.crt",
		"controller.key",
		"controller.crt",
		"scheduler.key",
		"scheduler.crt",
	}
	if cfg.Shared.KubeServiceAccountKey != "" {
		files = append(files, "service_account.key")
	}
	if cfg.Shared.KubeServiceAccountPrivateKey != "" {
		files = append(files, "service_account_private.key")
	}
	return files
}

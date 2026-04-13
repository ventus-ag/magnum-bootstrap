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

	if !req.Apply {
		if caNeedsRefresh {
			changes = append(changes, plannedFileChange(caCertPath, "reconcile cluster CA certificate"))
		}
		// Dry-run: report which certs would be generated or replaced.
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
			return moduleapi.Result{}, fmt.Errorf("master-certificates: %w", err)
		}
	}

	// Fetch CA cert if missing or unhealthy.
	if caNeedsRefresh {
		caPEM, err := client.FetchCACert(token)
		if err != nil {
			return moduleapi.Result{}, fmt.Errorf("master-certificates: %w", err)
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

	// Write service account keys.
	if cfg.Shared.KubeServiceAccountKey != "" {
		fileResult, err := (hostresource.FileSpec{Path: certDir + "/service_account.key", Content: []byte(cfg.Shared.KubeServiceAccountKey + "\n"), Mode: 0o440}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, fileResult.Changes...)
	}
	if cfg.Shared.KubeServiceAccountPrivateKey != "" {
		fileResult, err := (hostresource.FileSpec{Path: certDir + "/service_account_private.key", Content: []byte(cfg.Shared.KubeServiceAccountPrivateKey + "\n"), Mode: 0o440}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, fileResult.Changes...)
	}

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

package certapimanager

import (
	"context"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/certutil"
	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

const (
	certDir    = "/etc/kubernetes/certs"
	caKeyPath  = certDir + "/ca.key"
	caCertPath = certDir + "/ca.crt"
)

// shouldWriteCAKey decides whether to (over)write ca.key from the CA_KEY
// heat-param. ca.key signs CSRs for kube-controller-manager and MUST be the
// private partner of the active ca.crt. Installing an empty or mismatched key
// makes kube-controller-manager crashloop on "tls: private key does not match
// public key" — a control-plane outage. A genuine CA rotation supplies a NEW
// key that matches the NEW ca.crt already on disk, so rotation still converges;
// a stale/empty param is rejected and the working key is preserved.
func shouldWriteCAKey(caKey string) (bool, string) {
	if strings.TrimSpace(caKey) == "" {
		return false, "CA_KEY heat-param is empty"
	}
	// CA cert not on disk yet (fresh node, certs not generated) → cannot verify
	// the pair; allow the write so cluster creation proceeds.
	if _, err := os.Stat(caCertPath); err != nil {
		return true, ""
	}
	if !certutil.KeyPEMMatchesCertFile([]byte(caKey+"\n"), caCertPath) {
		return false, "CA_KEY does not match the on-disk ca.crt"
	}
	return true, ""
}

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "cert-api-manager" }
func (Module) Dependencies() []string { return []string{"master-certificates"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if !cfg.Shared.CertManagerAPI {
		return moduleapi.Result{}, nil
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	dirResult, err := (hostresource.DirectorySpec{Path: certDir, Mode: 0o550}).Apply(executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, dirResult.Changes...)

	// Write the CA private key from heat-params — but only when it is safe (see
	// shouldWriteCAKey): a missing/empty or mismatched key would crashloop
	// kube-controller-manager. A rotation supplies a matching new key, so the
	// rotated control plane still converges.
	if write, reason := shouldWriteCAKey(cfg.Shared.CAKey); write {
		fileResult, err := (hostresource.FileSpec{Path: caKeyPath, Content: []byte(cfg.Shared.CAKey + "\n"), Mode: 0o400}).Apply(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, fileResult.Changes...)
	} else if req.Logger != nil {
		req.Logger.Warnf("cert-api-manager: not writing ca.key from heat-params (%s); preserving existing key so kube-controller-manager keeps a CA key matching ca.crt", reason)
	}

	// Nothing to own if no key was ever written (skipped on a fresh node with an
	// empty param). Otherwise keep ownership idempotent.
	if _, statErr := os.Stat(caKeyPath); statErr != nil {
		return moduleapi.Result{Changes: changes, Outputs: map[string]string{"certManagerApi": "true"}}, nil
	}

	// ca.key sits alongside the master certs, which master-certificates owns
	// recursively as kube:kube_etcd. FileSpec writes as root, so match that
	// ownership here. Otherwise master-certificates finds this root-owned file on
	// the next reconcile, re-chowns the whole cert dir, reports it as changed
	// certificate material and needlessly restarts the entire control plane — i.e.
	// the reconcile never converges to zero changes (idempotency break, and a
	// control-plane restart on every periodic run in production).
	ownResult, err := (hostresource.OwnershipSpec{Path: caKeyPath, Owner: "kube", Group: "kube_etcd"}).Apply(executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, ownResult.Changes...)

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"certManagerApi": "true"},
	}, nil
}

// Destroy removes the CA private key.
func (Module) Destroy(_ context.Context, _ config.Config, req moduleapi.Request) error {
	if req.Logger != nil {
		req.Logger.Infof("cert-api-manager destroy: removing /etc/kubernetes/certs/ca.key")
	}
	_ = os.Remove("/etc/kubernetes/certs/ca.key")

	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:CertApiManager", name, res, opts...); err != nil {
		return nil, err
	}
	if heat.Cfg.Shared.CertManagerAPI {
		childOpts := hostresource.ChildResourceOptions(res, opts...)
		dirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-cert-dir", hostresource.DirectorySpec{Path: certDir, Mode: 0o550}, childOpts...)
		if err != nil {
			return nil, err
		}
		// Same safety gate as Run(): never register a ca.key write that would
		// install an empty/mismatched key over a working one.
		if write, _ := shouldWriteCAKey(heat.Cfg.Shared.CAKey); write {
			fileOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, dirRes)
			if _, err := hostsdk.RegisterFileSpec(ctx, name+"-ca-key", hostresource.FileSpec{Path: caKeyPath, Content: []byte(heat.Cfg.Shared.CAKey + "\n"), Mode: 0o400}, fileOpts...); err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"certManagerApi": pulumi.Bool(heat.Cfg.Shared.CertManagerAPI),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

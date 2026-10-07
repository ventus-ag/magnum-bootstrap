package kubecommon

import (
	"fmt"
	"os"

	"github.com/ventus-ag/magnum-bootstrap/internal/certutil"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

// SigningCACertPath holds the single CA certificate that pairs with ca.key, for
// kube-controller-manager's --cluster-signing-cert-file.
//
// It exists because ca.crt is a TRUST BUNDLE: a dual-CA rotation deliberately
// makes it carry new+old while both must be trusted. The CSR signing controller
// rejects a bundle ("expected 1 certificate, found 2") and kube-controller-manager
// then exits 1 on every start — the whole prepare→cutover window, which is
// unbounded when a rotation stalls. Splitting the signing anchor out of the trust
// anchor keeps signing valid whatever ca.crt holds.
const SigningCACertPath = "/etc/kubernetes/certs/ca-signing.crt"

// signingCACertPath is the write target; a var so tests can redirect it.
var signingCACertPath = SigningCACertPath

// EnsureSigningCA derives SigningCACertPath from the live trust bundle and the
// CA private key. It is a no-op when either input is absent (a worker, or
// cert_manager_api disabled — the signing flags are not set there either).
//
// When no certificate in the bundle pairs with the key, the first one is
// installed and the mismatch reported: that is exactly the pre-existing
// behaviour of pointing the flag at ca.crt, so a broken pair is never made worse
// here, and the CA-pair guards in the certificate modules are what repair it.
func EnsureSigningCA(executor *host.Executor, caBundlePath, caKeyPath string) ([]host.Change, string, error) {
	bundle, err := os.ReadFile(caBundlePath)
	if err != nil {
		return nil, "", nil
	}
	key, err := os.ReadFile(caKeyPath)
	if err != nil {
		return nil, "", nil
	}

	warning := ""
	signing, ok := certutil.CertMatchingKeyPEM(bundle, key)
	if !ok {
		signing = firstCertPEM(bundle)
		warning = fmt.Sprintf("no certificate in %s pairs with %s; kube-controller-manager will not be able to sign CSRs until the CA pair is repaired", caBundlePath, caKeyPath)
	}
	if len(signing) == 0 {
		return nil, warning, nil
	}

	result, err := (hostresource.FileSpec{Path: signingCACertPath, Content: signing, Mode: 0o444}).Apply(executor)
	if err != nil {
		return nil, warning, err
	}
	return result.Changes, warning, nil
}

func firstCertPEM(bundle []byte) []byte {
	single, _ := certutil.CapCertBundle(bundle, 1)
	return single
}

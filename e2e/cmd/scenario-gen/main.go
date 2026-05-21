// Command scenario-gen renders an /etc/sysconfig/heat-params file for one e2e
// node+operation, using the same scenario library the Go tests validate. The
// bash harness shells out to this so there is exactly one source of truth for
// what a "create" / "upgrade" / "resize" / "ca-rotate" heat-params looks like.
//
// CA key is read from -ca-key-file (produced by `mock-magnum -gen-ca`). A
// service-account RSA keypair is generated and written to -sa-key-file /
// -sa-priv-file if they don't already exist, so re-runs of the same node reuse
// the same SA keys (idempotency).
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/ventus-ag/magnum-bootstrap/e2e/scenario"
)

func main() {
	var (
		role         = flag.String("role", "master", "master|worker")
		op           = flag.String("op", "create", "create|upgrade|resize|ca-rotate")
		clusterName  = flag.String("cluster", "e2e", "cluster name (instance name prefix)")
		nodeIndex    = flag.Int("node-index", 0, "node index (master-0, minion-0, ...)")
		nodeIP       = flag.String("node-ip", "", "KUBE_NODE_IP (required)")
		masterIP     = flag.String("master-ip", "", "worker: API server IP to join (defaults to node-ip)")
		kubeTag      = flag.String("kube-tag", "v1.30.5", "Kubernetes version tag")
		clusterUUID  = flag.String("cluster-uuid", "11111111-1111-1111-1111-111111111111", "Magnum cluster UUID")
		authURL      = flag.String("auth-url", "http://127.0.0.1:9511/v3", "Keystone auth URL")
		magnumURL    = flag.String("magnum-url", "http://127.0.0.1:9511/v1", "Magnum API URL")
		caKeyFile    = flag.String("ca-key-file", "ca.key", "path to CA private key PEM (must match mock CA)")
		saKeyFile    = flag.String("sa-key-file", "sa.pub", "path to service-account public key PEM (generated if missing)")
		saPrivFile   = flag.String("sa-priv-file", "sa.key", "path to service-account private key PEM (generated if missing)")
		caRotationID = flag.String("ca-rotation-id", "", "CA rotation id (only meaningful with -op ca-rotate)")
		cloud        = flag.Bool("cloud-provider", false, "enable OpenStack cloud provider + OCCM + Cinder/Manila CSI (real OpenStack only)")
		usePodman    = flag.Bool("use-podman", true, "run master control-plane components via podman")
		recVersion   = flag.String("reconciler-version", "e2e", "RECONCILER_VERSION")
		recURL       = flag.String("reconciler-binary-url", "", "RECONCILER_BINARY_URL (e.g. file:///opt/e2e/bootstrap)")
		recSHA       = flag.String("reconciler-sha256", "", "RECONCILER_BINARY_URL_SHA256")
		out          = flag.String("o", "-", "output file ('-' for stdout)")
	)
	flag.Parse()

	if *nodeIP == "" {
		log.Fatal("scenario-gen: -node-ip is required")
	}
	mIP := *masterIP
	if mIP == "" {
		mIP = *nodeIP
	}

	caKey, err := os.ReadFile(*caKeyFile)
	if err != nil {
		log.Fatalf("scenario-gen: read ca key: %v", err)
	}
	saPub, saPriv, err := loadOrCreateRSAKeypair(*saKeyFile, *saPrivFile)
	if err != nil {
		log.Fatalf("scenario-gen: service-account keypair: %v", err)
	}

	cfg := scenario.Config{
		ClusterName:               *clusterName,
		Role:                      scenario.Role(*role),
		NodeIndex:                 *nodeIndex,
		Operation:                 scenario.Operation(*op),
		NodeIP:                    *nodeIP,
		MasterIP:                  mIP,
		KubeTag:                   *kubeTag,
		CARotationID:              *caRotationID,
		AuthURL:                   *authURL,
		MagnumURL:                 *magnumURL,
		ClusterUUID:               *clusterUUID,
		CAKey:                     string(caKey),
		SAKey:                     saPub,
		SAPrivateKey:              saPriv,
		CloudProvider:             *cloud,
		UsePodman:                 *usePodman,
		ReconcilerVersion:         *recVersion,
		ReconcilerBinaryURL:       *recURL,
		ReconcilerBinaryURLSHA256: *recSHA,
	}

	content := cfg.HeatParams()
	if *out == "-" {
		fmt.Print(content)
		return
	}
	if err := os.WriteFile(*out, []byte(content), 0o600); err != nil {
		log.Fatalf("scenario-gen: write %s: %v", *out, err)
	}
}

func loadOrCreateRSAKeypair(pubPath, privPath string) (pubPEM, privPEM string, err error) {
	pub, pubErr := os.ReadFile(pubPath)
	priv, privErr := os.ReadFile(privPath)
	if pubErr == nil && privErr == nil {
		return string(pub), string(priv), nil
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	privBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	pubBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	if err := os.WriteFile(privPath, privBytes, 0o600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(pubPath, pubBytes, 0o644); err != nil { //nolint:gosec
		return "", "", err
	}
	return string(pubBytes), string(privBytes), nil
}

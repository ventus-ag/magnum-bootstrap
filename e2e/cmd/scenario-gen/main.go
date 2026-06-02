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
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

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
		apiIP        = flag.String("api-ip", "", "master: KUBE_API_PRIVATE/PUBLIC_ADDRESS — the api_lb VIP in multi-master (defaults to node-ip)")
		etcdLBVIP    = flag.String("etcd-lb-vip", "", "master: ETCD_LB_VIP — the etcd_lb VIP in multi-master (empty = single-master bootstrap)")
		numMasters   = flag.Int("number-of-masters", 1, "master: NUMBER_OF_MASTERS")
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

		// Heat SoftwareDeployment ("agent") emit mode: render the metadata the
		// real heat-container-agent consumes instead of a heat-params file. The
		// `config` is the four REAL bootstrap scripts from -scripts-dir, exactly
		// as kubemaster.yaml/kubeminion.yaml concatenate them.
		emit       = flag.String("emit", "heat-params", "heat-params|deployment")
		scriptsDir = flag.String("scripts-dir", "", "deployment mode: dir holding the real bootstrap/*.sh from the cloned magnum repo")
		deployID   = flag.String("deploy-id", "", "deployment mode: SoftwareDeployment id (must be unique per trigger; default generated)")
		deployAct  = flag.String("deploy-action", "CREATE", "deployment mode: CREATE|UPDATE")
		signalID   = flag.String("signal-id", "", "deployment mode: deploy_signal_id URL the agent POSTs results to")
		deployRes  = flag.String("deploy-resource", "", "deployment mode: deploy_resource_name (default <role>_config_deployment)")
		deployStk  = flag.String("deploy-stack-id", "e2e-stack", "deployment mode: deploy_stack_id")
		deploySrv  = flag.String("deploy-server-id", "e2e-server", "deployment mode: deploy_server_id")
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
		APIIP:                     *apiIP,
		EtcdLBVIP:                 *etcdLBVIP,
		NumberOfMasters:           *numMasters,
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

	var content string
	switch *emit {
	case "heat-params":
		content = cfg.HeatParams()
	case "deployment":
		if *scriptsDir == "" {
			log.Fatal("scenario-gen: -scripts-dir is required with -emit deployment")
		}
		content, err = renderDeployment(cfg, deployOpts{
			scriptsDir: *scriptsDir,
			id:         *deployID,
			action:     *deployAct,
			signalID:   *signalID,
			resource:   *deployRes,
			stackID:    *deployStk,
			serverID:   *deploySrv,
		})
		if err != nil {
			log.Fatalf("scenario-gen: render deployment: %v", err)
		}
	default:
		log.Fatalf("scenario-gen: unknown -emit %q (want heat-params|deployment)", *emit)
	}

	if *out == "-" {
		fmt.Print(content)
		return
	}
	if err := os.WriteFile(*out, []byte(content), 0o600); err != nil {
		log.Fatalf("scenario-gen: write %s: %v", *out, err)
	}
}

// Heat SoftwareDeployment metadata shapes, matching what os-apply-config renders
// to /var/run/heat-config/heat-config and 55-heat-config / the script hook read.
type depInput struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type depOutput struct {
	Name        string `json:"name"`
	ErrorOutput bool   `json:"error_output,omitempty"`
}

type deployment struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Group   string         `json:"group"`
	Config  string         `json:"config"`
	Inputs  []depInput     `json:"inputs"`
	Outputs []depOutput    `json:"outputs"`
	Options map[string]any `json:"options"`
}

type metadataDoc struct {
	Deployments []deployment `json:"deployments"`
}

type deployOpts struct {
	scriptsDir, id, action, signalID, resource, stackID, serverID string
}

// renderDeployment builds the {"deployments":[...]} metadata the real
// heat-container-agent fetches. config is the concatenation of the REAL
// bootstrap scripts (role-specific write-heat-params first), and the ~90 inputs
// are passed as env vars, exactly like Heat's master_config/minion_config.
func renderDeployment(cfg scenario.Config, o deployOpts) (string, error) {
	var files []string
	if cfg.Role == scenario.RoleWorker {
		files = []string{"write-heat-params.sh"}
	} else {
		files = []string{"write-heat-params-master.sh"}
	}
	files = append(files, "install-reconciler-launcher.sh", "install-reconciler-systemd.sh", "run-reconciler-once.sh")

	var parts []string
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(o.scriptsDir, f))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", f, err)
		}
		parts = append(parts, string(b))
	}
	config := strings.Join(parts, "\n")

	inputs := make([]depInput, 0, len(cfg.Inputs())+8)
	for _, kv := range cfg.Inputs() {
		inputs = append(inputs, depInput{Name: kv.Name, Value: kv.Value})
	}
	// Transport inputs Heat injects; heat-config-notify reads deploy_signal_id.
	resource := o.resource
	if resource == "" {
		resource = string(cfg.Role) + "_config_deployment"
	}
	inputs = append(inputs,
		depInput{"deploy_signal_id", o.signalID},
		depInput{"deploy_signal_verb", "POST"},
		depInput{"deploy_signal_transport", "HEAT_SIGNAL"},
		depInput{"deploy_action", o.action},
		depInput{"deploy_stack_id", o.stackID},
		depInput{"deploy_resource_name", resource},
		depInput{"deploy_server_id", o.serverID},
	)

	id := o.id
	if id == "" {
		id = fmt.Sprintf("%s-%s-%d", cfg.InstanceName(), cfg.Operation, time.Now().Unix())
	}

	doc := metadataDoc{Deployments: []deployment{{
		ID:     id,
		Name:   resource,
		Group:  "script",
		Config: config,
		Inputs: inputs,
		Outputs: []depOutput{
			{Name: "reconcile_status"},
			{Name: "reconcile_step"},
			{Name: "reconcile_summary"},
			{Name: "reconcile_reason"},
			{Name: "reconcile_error_code"},
			{Name: "reconcile_failure", ErrorOutput: true},
		},
		Options: map[string]any{},
	}}}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
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

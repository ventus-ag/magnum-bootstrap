// Command magnum-e2e drives a real OpenStack Magnum cluster through the
// reconciler's full lifecycle, end to end, using the gophercloud SDK (no
// `openstack`/`kubectl` CLIs required — it is a single static binary).
//
// It is the only tier that exercises the OpenStack-integrated pieces the FCoS
// mock cannot fake: the cloud controller manager (LoadBalancer Services via
// Octavia) and Cinder CSI (dynamic PVCs). It walks a real cluster through:
//
//	create -> smoke (nodes Ready) -> cloud-integration (Cinder PVC + OCCM LB)
//	       -> upgrade -> resize -> ca-rotate -> delete
//
// Auth is standard OpenStack environment variables, read by gophercloud's
// openstack.AuthOptionsFromEnv() — either an application credential
// (OS_APPLICATION_CREDENTIAL_ID/SECRET) or user/password
// (OS_USERNAME/OS_PASSWORD/OS_PROJECT_NAME/OS_*_DOMAIN_NAME), plus OS_AUTH_URL
// and OS_REGION_NAME. No clouds.yaml. The Magnum service must already run the
// forked magnum_victoria driver; this tool only drives its API.
//
// Modes:
//
//	magnum-e2e            run the full lifecycle (default)
//	magnum-e2e -preflight authenticate + verify the template/keypair, then exit
//	magnum-e2e -teardown  delete the named cluster and exit (CI safety net)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// config is the full run configuration, populated from flags that each default
// from the matching environment variable (so CI can drive it purely via env,
// while a human can override any single knob on the command line).
type config struct {
	clusterName       string
	template          string // create-time template (name or UUID)
	upgradeTemplate   string // upgrade target template (defaults to template)
	keypair           string
	kubeTag           string
	kubeTagUpgrade    string
	nodeCount         int
	nodeCountResize   int
	masterCount       int
	reconcilerVersion string
	reconcilerURL     string
	bootstrapBinary   string // local reconciler binary to stage into Swift (current build)
	extraLabels       string
	timeoutMin        int
	region            string
	keepCluster       bool
	skipUpgrade       bool
	skipResize        bool
	skipCARotate      bool
}

// mode flags
var (
	flagPreflight     = flag.Bool("preflight", false, "authenticate + verify template/keypair reachability, then exit")
	flagTeardown      = flag.Bool("teardown", false, "delete the named cluster and exit (no lifecycle run)")
	flagList          = flag.Bool("list", false, "list cluster templates + keypairs visible to the project, then exit")
	flagClusters      = flag.Bool("clusters", false, "list all Magnum clusters + their status (diagnostic), then exit")
	flagStageSelftest = flag.Bool("stage-selftest", false, "stage -bootstrap-binary into Swift, fetch it back anonymously, verify, unstage, then exit")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true" || v == "yes"
}

func loadConfig() config {
	var c config
	// Cluster name default mirrors the old bash: recon-e2e-<timestamp>. This is
	// a normal binary (not a Workflow script), so time.Now() is fine here.
	defName := envOr("CLUSTER_NAME", "recon-e2e-"+time.Now().UTC().Format("20060102-150405"))

	flag.StringVar(&c.clusterName, "cluster-name", defName, "cluster name [CLUSTER_NAME]")
	flag.StringVar(&c.template, "template", envOr("CLUSTER_TEMPLATE", ""), "Magnum cluster template name or UUID [CLUSTER_TEMPLATE]")
	flag.StringVar(&c.upgradeTemplate, "upgrade-template", envOr("UPGRADE_TEMPLATE", ""), "upgrade target template (default: same as -template) [UPGRADE_TEMPLATE]")
	flag.StringVar(&c.keypair, "keypair", envOr("KEYPAIR", ""), "nova keypair name [KEYPAIR]")
	flag.StringVar(&c.kubeTag, "kube-tag", envOr("KUBE_TAG", ""), "kube_tag label override; empty = inherit the template's own kube_tag [KUBE_TAG]")
	flag.StringVar(&c.kubeTagUpgrade, "kube-tag-upgrade", envOr("KUBE_TAG_UPGRADE", ""), "upgrade target version label (informational; version comes from -upgrade-template) [KUBE_TAG_UPGRADE]")
	flag.IntVar(&c.nodeCount, "node-count", envIntOr("NODE_COUNT", 1), "initial worker count [NODE_COUNT]")
	flag.IntVar(&c.nodeCountResize, "node-count-resize", envIntOr("NODE_COUNT_RESIZE", 2), "resize target worker count [NODE_COUNT_RESIZE]")
	flag.IntVar(&c.masterCount, "master-count", envIntOr("MASTER_COUNT", 1), "master count [MASTER_COUNT]")
	flag.StringVar(&c.reconcilerVersion, "reconciler-version", envOr("RECONCILER_VERSION", ""), "reconciler_version label override [RECONCILER_VERSION]")
	flag.StringVar(&c.reconcilerURL, "reconciler-binary-url", envOr("RECONCILER_BINARY_URL", ""), "reconciler_binary_url label override [RECONCILER_BINARY_URL]")
	flag.StringVar(&c.bootstrapBinary, "bootstrap-binary", envOr("BOOTSTRAP_BINARY", ""), "path to a locally-built reconciler binary; staged into Swift (public-read) so nodes fetch this exact build [BOOTSTRAP_BINARY]")
	flag.StringVar(&c.extraLabels, "extra-labels", envOr("EXTRA_LABELS", ""), "extra cluster labels k=v,k2=v2 [EXTRA_LABELS]")
	flag.IntVar(&c.timeoutMin, "timeout-min", envIntOr("TIMEOUT_MIN", 60), "per-operation timeout in minutes [TIMEOUT_MIN]")
	flag.StringVar(&c.region, "region", envOr("OS_REGION_NAME", ""), "OpenStack region [OS_REGION_NAME]")
	keep := flag.Bool("keep", envBool("KEEP_CLUSTER"), "do not delete the cluster on exit [KEEP_CLUSTER]")
	skipUp := flag.Bool("skip-upgrade", envBool("SKIP_UPGRADE"), "skip the upgrade step [SKIP_UPGRADE]")
	skipRz := flag.Bool("skip-resize", envBool("SKIP_RESIZE"), "skip the resize step [SKIP_RESIZE]")
	skip := flag.Bool("skip-ca-rotate", envBool("SKIP_CA_ROTATE"), "skip the ca-rotate step [SKIP_CA_ROTATE]")

	flag.Parse()

	c.keepCluster = *keep
	c.skipUpgrade = *skipUp
	c.skipResize = *skipRz
	c.skipCARotate = *skip
	if c.upgradeTemplate == "" {
		c.upgradeTemplate = c.template
	}
	return c
}

func main() {
	cfg := loadConfig()

	// One signal-cancellable context for the whole run; SIGINT/SIGTERM trigger a
	// graceful teardown via the cleanup path rather than orphaning the cluster.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r, err := newRunner(ctx, cfg)
	if err != nil {
		die("auth/init: %v", err)
	}

	switch {
	case *flagList:
		if err := r.listResources(ctx); err != nil {
			die("list: %v", err)
		}
		return
	case *flagClusters:
		if err := r.listClusters(ctx); err != nil {
			die("clusters: %v", err)
		}
		return
	case *flagStageSelftest:
		if err := r.stageSelfTest(ctx); err != nil {
			die("stage-selftest: %v", err)
		}
		return
	case *flagTeardown:
		if err := r.deleteCluster(ctx); err != nil {
			die("teardown: %v", err)
		}
		r.log("teardown complete")
		return
	case *flagPreflight:
		if err := r.preflight(ctx); err != nil {
			die("preflight: %v", err)
		}
		r.log("preflight OK ✅")
		return
	}

	if err := r.run(ctx); err != nil {
		// On any failure dump the cluster's last status/faults, then tear down
		// (unless -keep) so a failed run does not leak billed resources.
		r.dumpClusterState(ctx)
		if cfg.keepCluster {
			r.log("KEEP_CLUSTER set — leaving %s in place for debugging", cfg.clusterName)
		} else if derr := r.deleteCluster(ctx); derr != nil {
			r.err("teardown after failure: %v", derr)
		}
		die("FAILED: %v", err)
	}

	if cfg.keepCluster {
		r.log("KEEP_CLUSTER set — leaving %s in place", cfg.clusterName)
	} else if err := r.deleteCluster(ctx); err != nil {
		die("teardown: %v", err)
	}
	r.log("ALL OPENSTACK SCENARIOS PASSED ✅")
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\033[1;31m[magnum] ERROR:\033[0m "+format+"\n", a...)
	os.Exit(1)
}

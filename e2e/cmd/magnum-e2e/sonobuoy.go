package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// runSonobuoy runs a Sonobuoy conformance test against the current cluster and
// records the outcome as the op's step. It:
//  1. writes an admin kubeconfig built from the Magnum-signed client cert,
//  2. locates the sonobuoy binary (SONOBUOY_BIN, then $PATH),
//  3. runs `sonobuoy run --wait` in the configured mode (SONOBUOY_MODE, default
//     "quick"; "certified-conformance" for the full weekly suite),
//  4. retrieves the results tarball into DIAG_DIR and extracts the plugin JUnit
//     so per-test results surface in the run artifact,
//  5. parses the plugin summary and fails the op if any test failed.
//
// Sonobuoy auto-selects the conformance image matching the cluster's Kubernetes
// version, so the same code covers every version the matrix creates.
func (r *runner) runSonobuoy(ctx context.Context) error {
	mode := envOr("SONOBUOY_MODE", "quick")
	version := firstNonEmpty(r.cfg.kubeTag, r.cfg.template, "cluster")
	r.log("sonobuoy: mode=%s version=%s", mode, version)

	bin, err := resolveSonobuoyBin()
	if err != nil {
		return err
	}

	_, restCfg, err := r.k8sClientWithConfig(ctx)
	if err != nil {
		return fmt.Errorf("build kube client for sonobuoy: %w", err)
	}
	kubeconfig, cleanup, err := writeKubeconfig(restCfg)
	if err != nil {
		return err
	}
	defer cleanup()

	// Per-mode wall-clock ceiling. quick is a single test (minutes); a full
	// conformance run is hundreds of tests (up to a couple of hours).
	runTimeout := 40 * time.Minute
	if mode != "quick" {
		runTimeout = 3 * time.Hour
	}
	runCtx, cancel := context.WithTimeout(ctx, runTimeout+15*time.Minute)
	defer cancel()

	// sonobuoy delete cleans the in-cluster aggregator + namespace. Run it before
	// (in case a prior attempt left state) and after (best-effort), so a retried
	// run-once does not collide with a leftover sonobuoy namespace.
	r.sonobuoyDelete(runCtx, bin, kubeconfig)
	defer r.sonobuoyDelete(context.Background(), bin, kubeconfig)

	runArgs := []string{
		"run", "--wait",
		"--kubeconfig", kubeconfig,
		"--mode", mode,
		"--timeout", strconv.Itoa(int(runTimeout.Seconds())),
	}
	// quick mode only makes sense for the e2e plugin; skip systemd-logs to keep
	// it fast. Full conformance keeps the default plugin set.
	if mode == "quick" {
		runArgs = append(runArgs, "--plugin", "e2e")
	}
	r.log("sonobuoy: %s %s", bin, strings.Join(runArgs, " "))
	if err := r.runStreaming(runCtx, bin, runArgs...); err != nil {
		return fmt.Errorf("sonobuoy run (mode=%s): %w", mode, err)
	}

	// Retrieve results tarball into DIAG_DIR (uploaded as a CI artifact).
	// `sonobuoy retrieve <dir>` writes an auto-named tarball INTO <dir> and prints
	// its full path on stdout — capture that path rather than guessing a name (the
	// -f flag is a bare filename, not a full path, and mis-joins if given one).
	diag := envOr("DIAG_DIR", "e2e-diagnostics")
	_ = os.MkdirAll(diag, 0o755)
	retrieved, err := r.runCapture(runCtx, bin, "retrieve", diag, "--kubeconfig", kubeconfig)
	if err != nil {
		return fmt.Errorf("sonobuoy retrieve: %w", err)
	}
	tarball, err := resolveRetrievedTarball(retrieved, diag)
	if err != nil {
		return fmt.Errorf("sonobuoy retrieve: %w", err)
	}
	// Normalize to a stable, version-tagged name for the artifact.
	named := filepath.Join(diag, "sonobuoy-"+sanitizeFilename(version)+".tar.gz")
	if named != tarball {
		if rerr := os.Rename(tarball, named); rerr == nil {
			tarball = named
		}
	}
	r.log("sonobuoy: results tarball -> %s", tarball)

	// Extract the plugin JUnit so the Tests tab / artifact shows each conformance
	// test (best-effort — never fail the op just because extraction hiccuped).
	junitDest := filepath.Join(diag, "junit-sonobuoy-"+sanitizeFilename(version)+".xml")
	if err := extractSonobuoyJUnit(tarball, junitDest); err != nil {
		r.log("sonobuoy: could not extract plugin JUnit: %v", err)
	} else {
		r.log("sonobuoy: plugin JUnit -> %s", junitDest)
	}

	// Parse the human-readable results summary and decide pass/fail.
	out, err := exec.CommandContext(runCtx, bin, "results", tarball, "--plugin", "e2e").CombinedOutput()
	if err != nil {
		return fmt.Errorf("sonobuoy results: %w (output: %s)", err, truncateOneLine(string(out), 300))
	}
	res, perr := parseSonobuoyResults(string(out))
	if perr != nil {
		return fmt.Errorf("parse sonobuoy results: %w", perr)
	}
	r.log("sonobuoy[%s]: status=%s total=%d passed=%d failed=%d skipped=%d",
		version, res.status, res.total, res.passed, res.failed, res.skipped)
	r.writeSonobuoySummary(version, mode, res)

	if res.status != "passed" || res.failed > 0 {
		return fmt.Errorf("sonobuoy conformance FAILED for %s: status=%s, %d/%d tests failed",
			version, res.status, res.failed, res.total)
	}
	return nil
}

// resolveSonobuoyBin returns the sonobuoy binary path: SONOBUOY_BIN if set, else
// whatever is on $PATH. The workflow installs it before invoking the driver.
func resolveSonobuoyBin() (string, error) {
	if b := os.Getenv("SONOBUOY_BIN"); b != "" {
		if _, err := os.Stat(b); err != nil {
			return "", fmt.Errorf("SONOBUOY_BIN=%q: %w", b, err)
		}
		return b, nil
	}
	p, err := exec.LookPath("sonobuoy")
	if err != nil {
		return "", fmt.Errorf("sonobuoy binary not found (set SONOBUOY_BIN or install it on PATH): %w", err)
	}
	return p, nil
}

// writeKubeconfig serializes a rest.Config (Magnum-signed admin cert) into a
// temporary kubeconfig file for the sonobuoy CLI. The returned cleanup removes it.
func writeKubeconfig(restCfg *rest.Config) (string, func(), error) {
	api := clientcmdapi.NewConfig()
	api.Clusters["cluster"] = &clientcmdapi.Cluster{
		Server:                   restCfg.Host,
		CertificateAuthorityData: restCfg.CAData,
	}
	api.AuthInfos["admin"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: restCfg.CertData,
		ClientKeyData:         restCfg.KeyData,
	}
	api.Contexts["default"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "admin"}
	api.CurrentContext = "default"

	f, err := os.CreateTemp("", "sonobuoy-kubeconfig-*.yaml")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temp kubeconfig: %w", err)
	}
	path := f.Name()
	_ = f.Close()
	if err := clientcmd.WriteToFile(*api, path); err != nil {
		_ = os.Remove(path)
		return "", func() {}, fmt.Errorf("write kubeconfig: %w", err)
	}
	return path, func() { _ = os.Remove(path) }, nil
}

// runStreaming runs a command with its stdout/stderr streamed to the driver's own
// output so long sonobuoy phases show live progress in the CI log.
func (r *runner) runStreaming(ctx context.Context, bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runCapture runs a command capturing stdout (returned) while streaming stderr to
// the CI log, so a command whose result is a value it prints (e.g. `sonobuoy
// retrieve` printing the tarball path) can be both watched and parsed.
func (r *runner) runCapture(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return string(out), err
}

// resolveRetrievedTarball turns `sonobuoy retrieve` stdout into the tarball path.
// retrieve prints the written path (usually the full path under dir; sometimes a
// bare filename). Take the last non-empty line and resolve it to an existing file
// under dir.
func resolveRetrievedTarball(stdout, dir string) (string, error) {
	var line string
	for _, l := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if s := strings.TrimSpace(l); s != "" {
			line = s
		}
	}
	if line == "" {
		return "", fmt.Errorf("retrieve printed no path")
	}
	candidates := []string{line, filepath.Join(dir, filepath.Base(line))}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("retrieved tarball %q not found under %q", line, dir)
}

// sonobuoyDelete tears down the in-cluster sonobuoy state (best-effort).
func (r *runner) sonobuoyDelete(ctx context.Context, bin, kubeconfig string) {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := exec.CommandContext(dctx, bin, "delete", "--wait", "--kubeconfig", kubeconfig).Run(); err != nil {
		r.log("sonobuoy delete (best-effort): %v", err)
	}
}

// sonobuoyResult is the parsed plugin summary.
type sonobuoyResult struct {
	status                         string
	total, passed, failed, skipped int
}

var (
	sonobuoyStatusRe = regexp.MustCompile(`(?mi)^\s*Status:\s*(\S+)`)
	sonobuoyCountRe  = regexp.MustCompile(`(?mi)^\s*(Total|Passed|Failed|Skipped):\s*(\d+)`)
)

// parseSonobuoyResults parses the `sonobuoy results --plugin e2e` summary block:
//
//	Plugin: e2e
//	Status: passed
//	Total: 1
//	Passed: 1
//	Failed: 0
//	Skipped: 0
func parseSonobuoyResults(out string) (sonobuoyResult, error) {
	var res sonobuoyResult
	if m := sonobuoyStatusRe.FindStringSubmatch(out); m != nil {
		res.status = strings.ToLower(m[1])
	}
	for _, m := range sonobuoyCountRe.FindAllStringSubmatch(out, -1) {
		n, _ := strconv.Atoi(m[2])
		switch strings.ToLower(m[1]) {
		case "total":
			res.total = n
		case "passed":
			res.passed = n
		case "failed":
			res.failed = n
		case "skipped":
			res.skipped = n
		}
	}
	if res.status == "" {
		return res, fmt.Errorf("no Status: line in sonobuoy results output")
	}
	return res, nil
}

// junitInTarRe matches the e2e plugin's JUnit file inside a results tarball.
var junitInTarRe = regexp.MustCompile(`plugins/e2e/results/.*junit.*\.xml$`)

// extractSonobuoyJUnit copies the e2e plugin's JUnit XML out of a results
// tarball to dest, so it can be uploaded/consumed independently.
func extractSonobuoyJUnit(tarball, dest string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("no e2e JUnit found in %s", tarball)
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || !junitInTarRe.MatchString(hdr.Name) {
			continue
		}
		out, err := os.Create(dest)
		if err != nil {
			return err
		}
		defer out.Close()
		if _, err := io.Copy(out, io.LimitReader(tr, 64<<20)); err != nil {
			return err
		}
		return nil
	}
}

// writeSonobuoySummary appends a compact conformance result table to
// $GITHUB_STEP_SUMMARY so the version's outcome is visible on the run page.
func (r *runner) writeSonobuoySummary(version, mode string, res sonobuoyResult) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	icon := "✅"
	if res.status != "passed" || res.failed > 0 {
		icon = "❌"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n### %s Sonobuoy conformance — %s (mode: %s)\n\n", icon, version, mode)
	b.WriteString("| Status | Total | Passed | Failed | Skipped |\n")
	b.WriteString("|---|---|---|---|---|\n")
	fmt.Fprintf(&b, "| %s | %d | %d | %d | %d |\n", res.status, res.total, res.passed, res.failed, res.skipped)
	appendFile(path, b.String())
}

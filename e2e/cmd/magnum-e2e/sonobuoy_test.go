package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestParseSonobuoyResultsPassed(t *testing.T) {
	out := `Plugin: e2e
Status: passed
Total: 1
Passed: 1
Failed: 0
Skipped: 0

Passed tests:
	[sig-network] ...
`
	res, err := parseSonobuoyResults(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.status != "passed" || res.total != 1 || res.passed != 1 || res.failed != 0 || res.skipped != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestParseSonobuoyResultsFailed(t *testing.T) {
	out := `Plugin: e2e
Status: failed
Total: 400
Passed: 397
Failed: 3
Skipped: 0
`
	res, err := parseSonobuoyResults(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.status != "failed" || res.failed != 3 || res.total != 400 || res.passed != 397 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestParseSonobuoyResultsNoStatus(t *testing.T) {
	if _, err := parseSonobuoyResults("garbage output with no status"); err == nil {
		t.Fatal("expected error when Status line is missing")
	}
}

func TestExtractSonobuoyJUnit(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "results.tar.gz")

	// Build a minimal results tarball with the plugin JUnit at the expected path.
	want := `<testsuite tests="1"></testsuite>`
	f, err := os.Create(tarball)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"plugins/e2e/sonobuoy_results.yaml":      "meta",
		"plugins/e2e/results/junit_01.xml":       want,
		"podlogs/kube-system/coredns/logs/x.txt": "noise",
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "junit-out.xml")
	if err := extractSonobuoyJUnit(tarball, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("extracted JUnit = %q, want %q", string(got), want)
	}
}

func TestResolveRetrievedTarball(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "202607181657_sonobuoy_abc.tar.gz")
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Full path on stdout (with trailing progress noise on other lines).
	got, err := resolveRetrievedTarball("uploading...\n"+real+"\n", dir)
	if err != nil || got != real {
		t.Fatalf("full-path: got (%q,%v), want %q", got, err, real)
	}

	// Bare filename on stdout → resolved against dir.
	got, err = resolveRetrievedTarball(filepath.Base(real)+"\n", dir)
	if err != nil || got != real {
		t.Fatalf("bare-name: got (%q,%v), want %q", got, err, real)
	}

	// Nothing printed / file absent → error.
	if _, err := resolveRetrievedTarball("", dir); err == nil {
		t.Fatal("expected error on empty stdout")
	}
	if _, err := resolveRetrievedTarball("nope.tar.gz\n", dir); err == nil {
		t.Fatal("expected error when named file does not exist")
	}
}

func TestExtractSonobuoyJUnitMissing(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "empty.tar.gz")
	f, _ := os.Create(tarball)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()
	if err := extractSonobuoyJUnit(tarball, filepath.Join(dir, "out.xml")); err == nil {
		t.Fatal("expected error when tarball has no JUnit")
	}
}

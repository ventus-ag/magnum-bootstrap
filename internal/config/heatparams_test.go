package config

import (
	"os"
	"strings"
	"testing"
)

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func TestParseHeatParamsBasics(t *testing.T) {
	raw, err := parseHeatParams(`
# comment
INSTANCE_NAME="c1-master-0"
KUBE_TAG=v1.31.4
EMPTY=""
SINGLE='quoted value'
SPACED = padded
`)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"INSTANCE_NAME": "c1-master-0",
		"KUBE_TAG":      "v1.31.4",
		"EMPTY":         "",
		"SINGLE":        "quoted value",
		"SPACED":        "padded",
	} {
		if got := raw[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestParseHeatParamsEscapedNewlineRoundTrip(t *testing.T) {
	// The fork escapes newlines before Heat writes the file
	// (k8s_fedora_template_def.py replace("\n","\\n")); Unquote must restore
	// them so PEM keys survive the round trip.
	raw, err := parseHeatParams(`CA_KEY="-----BEGIN RSA PRIVATE KEY-----\nabc\ndef\n-----END RSA PRIVATE KEY-----\n"`)
	if err != nil {
		t.Fatal(err)
	}
	want := "-----BEGIN RSA PRIVATE KEY-----\nabc\ndef\n-----END RSA PRIVATE KEY-----\n"
	if raw["CA_KEY"] != want {
		t.Fatalf("CA_KEY = %q", raw["CA_KEY"])
	}
}

func TestParseHeatParamsToleratesStrayLines(t *testing.T) {
	// A hand-edited stray line must not brick every future reconcile.
	raw, err := parseHeatParams(`
KUBE_TAG=v1.31.4
this line has no equals sign
=novalue-key
NODEGROUP_ROLE=worker
`)
	if err != nil {
		t.Fatalf("stray lines must be tolerated, got: %v", err)
	}
	if raw["KUBE_TAG"] != "v1.31.4" || raw["NODEGROUP_ROLE"] != "worker" {
		t.Fatalf("valid keys lost: %+v", raw)
	}
}

func TestParseHeatParamsLongValue(t *testing.T) {
	// Values beyond bufio.Scanner's default 64KiB token limit must parse.
	long := strings.Repeat("x", 300*1024)
	raw, err := parseHeatParams("BIG=" + long + "\nKUBE_TAG=v1.31.4\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw["BIG"]) != 300*1024 {
		t.Fatalf("BIG length %d", len(raw["BIG"]))
	}
	if raw["KUBE_TAG"] != "v1.31.4" {
		t.Fatal("key after long value lost")
	}
}

func TestParseHeatParamsCRLF(t *testing.T) {
	raw, err := parseHeatParams("KUBE_TAG=v1.31.4\r\nNODEGROUP_ROLE=master\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if raw["KUBE_TAG"] != "v1.31.4" || raw["NODEGROUP_ROLE"] != "master" {
		t.Fatalf("CRLF parse: %+v", raw)
	}
}

func TestDecodeValueMalformedQuotesKeepRaw(t *testing.T) {
	// A value strconv.Unquote rejects (inner quote, non-Go escape) falls back
	// to the raw string; documenting current behavior so a change is loud.
	got := decodeValue(`"--foo=\d"`)
	if got != `"--foo=\d"` {
		t.Fatalf("got %q", got)
	}
}

func TestUnknownRoleDefaultsToWorker(t *testing.T) {
	raw, err := parseHeatParams(`
NODEGROUP_ROLE=gpu-pool
KUBE_MASTER_IP=10.0.0.5
`)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw // parse-level check above; full Load role check below

	dir := t.TempDir()
	path := dir + "/heat-params"
	if err := writeTestFile(path, "NODEGROUP_ROLE=gpu-pool\nKUBE_MASTER_IP=10.0.0.5\nINSTANCE_NAME=c1-gpu-0\nKUBE_TAG=v1.31.4\n"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Role() != RoleWorker {
		t.Fatalf("custom role must default to worker, got %s", cfg.Role())
	}
	if cfg.Worker == nil || cfg.Worker.KubeMasterIP != "10.0.0.5" {
		t.Fatalf("worker config not populated: %+v", cfg.Worker)
	}
}

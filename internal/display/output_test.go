package display

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
)

func TestPrintDetailedDiffFallsBackToOutputsForComponentResources(t *testing.T) {
	var out bytes.Buffer
	renderer := NewRenderer(&out, false)

	renderer.printDetailedDiff(apitype.StepEventMetadata{
		Old: &apitype.StepEventStateMetadata{
			Outputs: map[string]interface{}{
				"kubeTag": "v1.32.0",
			},
		},
		New: &apitype.StepEventStateMetadata{
			Outputs: map[string]interface{}{
				"kubeTag": "v1.35.0",
			},
		},
		DetailedDiff: map[string]apitype.PropertyDiff{
			"kubeTag": {Kind: apitype.DiffUpdate},
		},
	})

	got := out.String()
	want := `~ kubeTag: "v1.32.0" => "v1.35.0"`
	if !strings.Contains(got, want) {
		t.Fatalf("expected diff output %q, got %q", want, got)
	}
	if strings.Contains(got, "<nil>") {
		t.Fatalf("expected resolved values, got %q", got)
	}
}

func TestPrintDetailedDiffUsesInputsForInputDiffs(t *testing.T) {
	var out bytes.Buffer
	renderer := NewRenderer(&out, false)

	renderer.printDetailedDiff(apitype.StepEventMetadata{
		Old: &apitype.StepEventStateMetadata{
			Inputs: map[string]interface{}{
				"kubeTag": "v1.32.0",
			},
			Outputs: map[string]interface{}{
				"kubeTag": "state-old",
			},
		},
		New: &apitype.StepEventStateMetadata{
			Inputs: map[string]interface{}{
				"kubeTag": "v1.35.0",
			},
			Outputs: map[string]interface{}{
				"kubeTag": "state-new",
			},
		},
		DetailedDiff: map[string]apitype.PropertyDiff{
			"kubeTag": {Kind: apitype.DiffUpdate, InputDiff: true},
		},
	})

	got := out.String()
	want := `~ kubeTag: "v1.32.0" => "v1.35.0"`
	if !strings.Contains(got, want) {
		t.Fatalf("expected input diff output %q, got %q", want, got)
	}
	if strings.Contains(got, "state-old") || strings.Contains(got, "state-new") {
		t.Fatalf("expected input values, got %q", got)
	}
}

func TestPrintDetailedDiffParsesIndexedPropertyPaths(t *testing.T) {
	var out bytes.Buffer
	renderer := NewRenderer(&out, false)

	renderer.printDetailedDiff(apitype.StepEventMetadata{
		Old: &apitype.StepEventStateMetadata{
			Outputs: map[string]interface{}{
				"checks": []interface{}{"role=master", "operation=create"},
			},
		},
		New: &apitype.StepEventStateMetadata{
			Outputs: map[string]interface{}{
				"checks": []interface{}{"role=master", "operation=upgrade"},
			},
		},
		DetailedDiff: map[string]apitype.PropertyDiff{
			"checks[1]": {Kind: apitype.DiffUpdate},
		},
	})

	got := out.String()
	want := `~ checks[1]: "operation=create" => "operation=upgrade"`
	if !strings.Contains(got, want) {
		t.Fatalf("expected indexed diff output %q, got %q", want, got)
	}
}

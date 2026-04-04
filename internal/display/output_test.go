package display

import (
	"bytes"
	"strings"
	"testing"

	autoevents "github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"

	"github.com/ventus-ag/magnum-bootstrap/internal/result"
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

func TestStreamEventsPrintsUpdateFromResOutputsWhenPreEventMissing(t *testing.T) {
	var out bytes.Buffer
	renderer := NewRenderer(&out, false)
	ch := make(chan autoevents.EngineEvent, 1)
	ch <- autoevents.EngineEvent{
		EngineEvent: apitype.EngineEvent{
			Timestamp: 1,
			ResOutputsEvent: &apitype.ResOutputsEvent{
				Metadata: apitype.StepEventMetadata{
					Op:   apitype.OpUpdate,
					Type: "magnum:module:InstallClients",
					URN:  "urn:pulumi:test::magnum-bootstrap::magnum:module:InstallClients::node-test-client-tools",
					Old: &apitype.StepEventStateMetadata{
						Outputs: map[string]interface{}{"kubeletUrl": "old"},
					},
					New: &apitype.StepEventStateMetadata{
						Outputs: map[string]interface{}{"kubeletUrl": "new"},
					},
					DetailedDiff: map[string]apitype.PropertyDiff{
						"kubeletUrl": {Kind: apitype.DiffUpdate},
					},
				},
			},
		},
	}
	close(ch)

	renderer.StreamEvents(ch)
	got := out.String()
	if !strings.Contains(got, "TYPE=magnum:module:InstallClients") {
		t.Fatalf("expected update line in output, got %q", got)
	}
	if !strings.Contains(got, `~ kubeletUrl: "old" => "new"`) {
		t.Fatalf("expected detailed diff in output, got %q", got)
	}
}

func TestPrintResultIncludesPreviewPlan(t *testing.T) {
	var out bytes.Buffer
	renderer := NewRenderer(&out, false)

	renderer.PrintResult(result.Result{
		Status:      "planned",
		Step:        "preview",
		Summary:     "planned 1 phase",
		PreviewPlan: "Previewing update\n  + create TYPE=magnum:module:ContainerRuntime",
	})

	got := out.String()
	if !strings.Contains(got, "Previewing update") {
		t.Fatalf("expected preview plan text, got %q", got)
	}
	if !strings.Contains(got, "TYPE=magnum:module:ContainerRuntime") {
		t.Fatalf("expected preview plan resource line, got %q", got)
	}
}

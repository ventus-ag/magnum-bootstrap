package display

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/result"
)

const (
	colorGreen   = "\x1b[32;1m"
	colorRed     = "\x1b[31;1m"
	colorYellow  = "\x1b[33;1m"
	colorBlue    = "\x1b[34;1m"
	colorMagenta = "\x1b[35;1m"
	colorCyan    = "\x1b[36;1m"
	colorGray    = "\x1b[90;1m"
	colorReset   = "\x1b[0m"
)

// pulumiEvent is a collected Pulumi resource event for deferred summary output.
type pulumiEvent struct {
	op       string
	resType  string
	detailed map[string]apitype.PropertyDiff
	meta     apitype.StepEventMetadata
}

type Renderer struct {
	writer     io.Writer
	enableANSI bool
	debug      bool
	prePrinted map[string]bool
	collected  []pulumiEvent
}

func NewRenderer(writer io.Writer, debug bool) *Renderer {
	return &Renderer{
		writer:     writer,
		enableANSI: supportsColor(writer),
		debug:      debug,
		prePrinted: make(map[string]bool),
	}
}

// StreamEvents processes Pulumi engine events. Resource-level events
// (create, update, replace, delete) are collected silently and printed
// later in the summary via PrintResult. Only errors and diagnostics are
// printed immediately so that failures are visible during the run.
func (r *Renderer) StreamEvents(ch <-chan events.EngineEvent) {
	if r == nil || r.writer == nil {
		for range ch {
		}
		return
	}

	for ev := range ch {
		if ev.EngineEvent.Timestamp == 0 {
			continue
		}
		switch {
		case ev.EngineEvent.ResourcePreEvent != nil:
			meta := ev.EngineEvent.ResourcePreEvent.Metadata
			if meta.Type == "pulumi:pulumi:Stack" {
				continue
			}
			op := string(meta.Op)
			if op == "same" {
				continue
			}
			r.markPrePrinted(meta)
			r.collected = append(r.collected, pulumiEvent{
				op:       op,
				resType:  string(meta.Type),
				detailed: meta.DetailedDiff,
				meta:     meta,
			})

		case ev.EngineEvent.ResOutputsEvent != nil:
			meta := ev.EngineEvent.ResOutputsEvent.Metadata
			if meta.Type == "pulumi:pulumi:Stack" {
				continue
			}
			op := string(meta.Op)
			if op == "same" {
				continue
			}
			// Skip duplicate from ResourcePreEvent.
			if r.consumePrePrinted(meta) {
				continue
			}
			r.collected = append(r.collected, pulumiEvent{
				op:       op,
				resType:  string(meta.Type),
				detailed: meta.DetailedDiff,
				meta:     meta,
			})

		case ev.EngineEvent.DiagnosticEvent != nil:
			diag := ev.EngineEvent.DiagnosticEvent
			if diag.Severity == "debug" && !r.debug {
				continue
			}
			msg := strings.TrimRight(diag.Message, "\n")
			if msg == "" {
				continue
			}
			switch diag.Severity {
			case "error":
				fmt.Fprintf(r.writer, "%s\n", r.colorize("  diag: "+msg, colorRed))
			case "warning":
				fmt.Fprintf(r.writer, "%s\n", r.colorize("  diag: "+msg, colorYellow))
			default:
				if r.debug {
					fmt.Fprintf(r.writer, "%s\n", r.colorize("  diag: "+msg, colorGray))
				}
			}

		case ev.EngineEvent.ResOpFailedEvent != nil:
			meta := ev.EngineEvent.ResOpFailedEvent.Metadata
			fmt.Fprintf(r.writer, "%s\n", r.colorize(
				fmt.Sprintf("  ! FAILED %s TYPE=%s", meta.Op, meta.Type), colorRed))
		}
	}
}

// printDetailedDiff renders property-level changes for update/replace operations.
// Shows old → new values for each changed property:
//
//	~ kubeTag: "v1.32.0" => "v1.35.0"
//	+ newField: "value"
//	- removedField
func (r *Renderer) printDetailedDiff(meta apitype.StepEventMetadata) {
	// Sort property paths for stable output.
	paths := make([]string, 0, len(meta.DetailedDiff))
	for path := range meta.DetailedDiff {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		diff := meta.DetailedDiff[path]
		// Skip internal/noisy fields.
		if strings.HasPrefix(path, "__") {
			continue
		}

		oldPreferred := diffValueInputs
		newPreferred := diffValueInputs
		if !diff.InputDiff {
			oldPreferred = diffValueOutputs
		}

		oldVal := formatValue(resolveDiffValue(meta.Old, path, oldPreferred))
		newVal := formatValue(resolveDiffValue(meta.New, path, newPreferred))

		switch diff.Kind {
		case apitype.DiffAdd, apitype.DiffAddReplace:
			fmt.Fprintf(r.writer, "%s\n", r.colorize(
				fmt.Sprintf("      + %s: %s", path, newVal), colorGreen))
		case apitype.DiffDelete, apitype.DiffDeleteReplace:
			fmt.Fprintf(r.writer, "%s\n", r.colorize(
				fmt.Sprintf("      - %s: %s", path, oldVal), colorRed))
		case apitype.DiffUpdate, apitype.DiffUpdateReplace:
			fmt.Fprintf(r.writer, "      %s %s: %s => %s\n",
				r.colorize("~", colorYellow),
				path,
				r.colorize(oldVal, colorRed),
				r.colorize(newVal, colorGreen))
		}
	}
}

type diffValueSource int

const (
	diffValueInputs diffValueSource = iota
	diffValueOutputs
)

func resolveDiffValue(state *apitype.StepEventStateMetadata, path string, preferred diffValueSource) interface{} {
	if state == nil {
		return nil
	}

	for _, source := range orderedDiffSources(preferred) {
		switch source {
		case diffValueInputs:
			if value, ok := getValueAtPath(state.Inputs, path); ok {
				return value
			}
		case diffValueOutputs:
			if value, ok := getValueAtPath(state.Outputs, path); ok {
				return value
			}
		}
	}

	return nil
}

func orderedDiffSources(preferred diffValueSource) []diffValueSource {
	if preferred == diffValueOutputs {
		return []diffValueSource{diffValueOutputs, diffValueInputs}
	}
	return []diffValueSource{diffValueInputs, diffValueOutputs}
}

// getValueAtPath resolves a Pulumi property path like "spec.template[0].name"
// against a JSON-like map and returns the plain Go value when present.
func getValueAtPath(m map[string]interface{}, path string) (interface{}, bool) {
	if len(m) == 0 {
		return nil, false
	}

	propertyPath, err := resource.ParsePropertyPath(path)
	if err != nil {
		return nil, false
	}

	value, ok := propertyPath.Get(resource.NewObjectProperty(resource.NewPropertyMapFromMap(m)))
	if !ok {
		return nil, false
	}

	return unwrapPropertyValue(value), true
}

func unwrapPropertyValue(v resource.PropertyValue) interface{} {
	for {
		switch {
		case v.IsSecret():
			v = v.SecretValue().Element
		case v.IsOutput():
			output := v.OutputValue()
			if !output.Known {
				return nil
			}
			v = output.Element
		case v.IsComputed():
			return nil
		default:
			return v.Mappable()
		}
	}
}

// formatValue renders a value for diff display, truncating long strings.
func formatValue(v interface{}) string {
	if v == nil {
		return "<nil>"
	}
	s := fmt.Sprintf("%v", v)
	// Truncate long values (e.g. base64 cert data).
	if len(s) > 80 {
		return fmt.Sprintf("%.77s...", s)
	}
	return fmt.Sprintf("%q", s)
}

// PrintResult renders the final result: Pulumi resource events, host operations,
// warnings, and a short summary — all in one block at the end of the run.
func (r *Renderer) PrintResult(res result.Result) {
	if r == nil || r.writer == nil {
		return
	}
	r.printPreviewPlan(res.PreviewPlan)
	r.printPulumiSummary(res.PulumiSummary)
	r.printCollectedEvents()
	r.printOperations(res.Operations)
	r.printWarnings(res.Warnings)
	r.printSummary(res)
}

// printCollectedEvents renders Pulumi resource events that were collected
// during StreamEvents.
func (r *Renderer) printCollectedEvents() {
	if len(r.collected) == 0 {
		return
	}
	fmt.Fprintln(r.writer)
	for _, ev := range r.collected {
		sigil := pulumiSigil(ev.op)
		color := pulumiColor(ev.op)
		name := shortResourceName(string(ev.meta.URN))
		fmt.Fprintf(r.writer, "%s\n", r.colorize(
			fmt.Sprintf("  %s %-8s %s  %s", sigil, ev.op, ev.resType, name), color))
		if (ev.op == "update" || ev.op == "replace") && len(ev.detailed) > 0 {
			r.printDetailedDiff(ev.meta)
		}
	}
}

// shortResourceName extracts the last name segment from a Pulumi URN.
// URN format: urn:pulumi:stack::project::type$type::name
// Returns just the final "name" part after the last "::".
func shortResourceName(urn string) string {
	if idx := strings.LastIndex(urn, "::"); idx >= 0 && idx+2 < len(urn) {
		return urn[idx+2:]
	}
	return urn
}

func (r *Renderer) printPulumiSummary(summary string) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return
	}
	fmt.Fprintln(r.writer)
	fmt.Fprintln(r.writer, r.colorize(summary, colorCyan))
}

func (r *Renderer) printPreviewPlan(plan string) {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return
	}
	fmt.Fprintln(r.writer)
	fmt.Fprintln(r.writer, plan)
}

func (r *Renderer) printOperations(changes []host.Change) {
	if len(changes) == 0 {
		return
	}

	fmt.Fprintln(r.writer)
	for _, change := range changes {
		fmt.Fprintf(r.writer, "  %s %s\n", r.formatAction(change), change.Summary)
	}

	counts := make(map[string]int)
	for _, change := range changes {
		counts[normalizeAction(change.Action)]++
	}

	order := []string{
		host.ActionCreate,
		host.ActionUpdate,
		host.ActionReplace,
		host.ActionDelete,
		host.ActionReload,
		host.ActionRestart,
		host.ActionRead,
		host.ActionOther,
	}

	fmt.Fprintln(r.writer)
	for _, action := range order {
		if counts[action] == 0 {
			continue
		}
		fmt.Fprintf(r.writer, "  %s %d\n", r.colorize(fmt.Sprintf("%-10s", actionLabel(action)), colorForAction(action)), counts[action])
	}
}

func (r *Renderer) markPrePrinted(meta apitype.StepEventMetadata) {
	if r == nil || r.prePrinted == nil {
		return
	}
	r.prePrinted[stepKey(meta)] = true
}

func (r *Renderer) consumePrePrinted(meta apitype.StepEventMetadata) bool {
	if r == nil || r.prePrinted == nil {
		return false
	}
	key := stepKey(meta)
	if !r.prePrinted[key] {
		return false
	}
	delete(r.prePrinted, key)
	return true
}

func stepKey(meta apitype.StepEventMetadata) string {
	return fmt.Sprintf("%s|%s|%s", meta.URN, meta.Type, meta.Op)
}

func (r *Renderer) printWarnings(warnings []string) {
	if len(warnings) == 0 {
		return
	}
	fmt.Fprintln(r.writer)
	for _, warning := range warnings {
		fmt.Fprintf(r.writer, "  %s %s\n", r.colorize("warning", colorYellow), warning)
	}
}

func (r *Renderer) printSummary(res result.Result) {
	fmt.Fprintln(r.writer)
	color := colorGreen
	if res.Status == "failed" {
		color = colorRed
	}
	if res.Status == "planned" {
		color = colorCyan
	}
	fmt.Fprintln(r.writer, r.colorize(res.Summary, color))
}

func (r *Renderer) formatAction(change host.Change) string {
	action := normalizeAction(change.Action)
	return r.colorize(fmt.Sprintf("%-4s %-8s", sigilForAction(action), action), colorForAction(action))
}

func normalizeAction(action string) string {
	switch action {
	case host.ActionCreate, host.ActionUpdate, host.ActionDelete, host.ActionReplace, host.ActionReload, host.ActionRestart, host.ActionRead:
		return action
	default:
		return host.ActionOther
	}
}

func sigilForAction(action string) string {
	switch action {
	case host.ActionCreate:
		return "+"
	case host.ActionUpdate:
		return "~"
	case host.ActionDelete:
		return "-"
	case host.ActionReplace:
		return "-/+"
	case host.ActionReload:
		return "L"
	case host.ActionRestart:
		return "R"
	case host.ActionRead:
		return "="
	default:
		return "?"
	}
}

// isVisibleOp returns true for operations that should appear in the event stream.
// Only update, replace, delete, and import represent real state changes worth
// showing. "create" is Pulumi state tracking noise (host-level creates are
// reported separately via module Run() results). "same" is no change.
func isVisibleOp(op string) bool {
	switch op {
	case "update", "replace", "delete", "import":
		return true
	}
	return false
}

func pulumiSigil(op string) string {
	switch op {
	case "create":
		return "+"
	case "update":
		return "+~"
	case "delete":
		return "-"
	case "replace":
		return "-/+"
	case "read":
		return "≈"
	case "import":
		return "="
	case "same":
		return "="
	default:
		return "?"
	}
}

func pulumiColor(op string) string {
	switch op {
	case "create":
		return colorGreen
	case "update":
		return colorBlue
	case "delete":
		return colorRed
	case "replace":
		return colorYellow
	case "read", "same":
		return colorGray
	case "import":
		return colorCyan
	default:
		return colorCyan
	}
}

func actionLabel(action string) string {
	switch action {
	case host.ActionCreate:
		return "Create:"
	case host.ActionUpdate:
		return "Update:"
	case host.ActionReplace:
		return "Replace:"
	case host.ActionDelete:
		return "Delete:"
	case host.ActionReload:
		return "Reload:"
	case host.ActionRestart:
		return "Restart:"
	case host.ActionRead:
		return "Read:"
	default:
		return "Other:"
	}
}

func colorForAction(action string) string {
	switch action {
	case host.ActionCreate:
		return colorGreen
	case host.ActionUpdate:
		return colorBlue
	case host.ActionReplace:
		return colorYellow
	case host.ActionDelete:
		return colorRed
	case host.ActionReload:
		return colorCyan
	case host.ActionRestart:
		return colorMagenta
	case host.ActionRead:
		return colorGray
	default:
		return colorGray
	}
}

func (r *Renderer) colorize(text, color string) string {
	if !r.enableANSI {
		return text
	}
	return color + text + colorReset
}

func supportsColor(writer io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}

	file, ok := writer.(*os.File)
	if !ok {
		return false
	}

	info, err := file.Stat()
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return false
	}

	term := os.Getenv("TERM")
	return term != "" && term != "dumb"
}

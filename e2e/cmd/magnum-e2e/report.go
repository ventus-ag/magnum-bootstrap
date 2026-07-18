package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type stepStatus string

const (
	stepPass stepStatus = "PASS"
	stepFail stepStatus = "FAIL"
	stepSkip stepStatus = "SKIP"
)

type stepResult struct {
	name   string
	desc   string
	status stepStatus
	dur    time.Duration
	errMsg string
}

// opDescriptions are one-line summaries of what each op proves, shown in the
// per-step run summary (stdout + the GitHub run summary + JUnit).
var opDescriptions = map[string]string{
	"upgrade":               "rolling k8s upgrade to the target version",
	"cloud-smoke":           "OCCM LB serves HTTP 200 + Cinder PVC create/resize",
	"autoscale":             "cluster-autoscaler scales workers up >2 then back down",
	"sonobuoy":              "Sonobuoy conformance (SONOBUOY_MODE, default quick)",
	"ca-rotate":             "rotate the cluster CA (dual-CA, zero-downtime)",
	"post-rotate":           "add a node after CA rotation (SA-key trust)",
	"verify-sa":             "service-account key consistency across nodes",
	"resize-workers":        "resize the worker nodegroup",
	"resize-masters":        "resize the master nodegroup",
	"add-nodepool":          "create an extra worker nodepool",
	"resize-nodepool":       "resize the extra worker nodepool",
	"del-nodepool":          "delete the extra worker nodepool",
	"disable-autoscaler":    "disable cluster-autoscaler and prune its Helm release",
	"enable-metrics-server": "enable metrics-server and wait for it to become Ready",
}

func opDescription(o op) string {
	if d, ok := opDescriptions[o.name]; ok {
		return d
	}
	return o.name
}

// opStepLabels builds the per-op summary labels. Upgrade rows include their
// target version (computed in the same order upgradeTarget() consumes the ladder)
// so a ladder run shows "upgrade→v1.28.4" rather than seven identical "upgrade".
func (r *runner) opStepLabels(ops []op) []string {
	labels := make([]string, len(ops))
	up := 0
	for i, o := range ops {
		lbl := formatOp(o)
		if o.name == "upgrade" {
			t := r.cfg.upgradeTemplate
			if up < len(r.ladder) {
				t = r.ladder[up]
			}
			lbl = "upgrade→" + t
			up++
		}
		labels[i] = lbl
	}
	return labels
}

// do runs one named step unless the run already failed (then it records SKIP and
// returns immediately). On failure it records FAIL, captures diagnostics at the
// failure point, and latches r.runFailed/r.runErr so later steps are skipped —
// giving an at-a-glance PASS/FAIL/SKIP summary instead of aborting silently.
func (r *runner) do(ctx context.Context, name, desc string, fn func() error) {
	if r.runFailed {
		r.steps = append(r.steps, stepResult{name: name, desc: desc, status: stepSkip})
		return
	}
	start := time.Now()
	err := fn()
	dur := time.Since(start)
	if err != nil {
		r.steps = append(r.steps, stepResult{name: name, desc: desc, status: stepFail, dur: dur, errMsg: err.Error()})
		r.runFailed = true
		r.runErr = fmt.Errorf("%s: %w", name, err)
		r.err("step %q FAILED: %v", name, err)
		r.collectDiagnostics(ctx, "step-"+name)
		r.collectHeatDiagnostics(ctx, "step-"+name)
		r.collectNodeLogs(ctx, "step-"+name)
		return
	}
	r.steps = append(r.steps, stepResult{name: name, desc: desc, status: stepPass, dur: dur})
}

func (r *runner) stepCounts() (pass, fail, skip int) {
	for _, s := range r.steps {
		switch s.status {
		case stepPass:
			pass++
		case stepFail:
			fail++
		case stepSkip:
			skip++
		}
	}
	return
}

// printRunSummary writes the per-step PASS/FAIL/SKIP table to the run log and, in
// CI, mirrors it to the GitHub run summary + a JUnit artifact. Always safe to call
// (no-op with no steps); call it once per scenario run, even on failure.
func (r *runner) printRunSummary() {
	if len(r.steps) == 0 {
		return
	}
	pass, fail, skip := r.stepCounts()
	r.log("════════════ STEP SUMMARY: %s ════════════", r.cfg.scenario)
	for i, s := range r.steps {
		r.log("  %2d. %-24s %s %-4s %7s  %s", i+1, s.name, statusIcon(s.status), s.status, s.dur.Round(time.Second), s.desc)
		if s.status == stepFail && s.errMsg != "" {
			r.log("      ↳ %s", truncateOneLine(s.errMsg, 240))
		}
	}
	r.log("  → %d passed, %d failed, %d skipped", pass, fail, skip)
	r.writeGitHubStepSummary()
	r.writeJUnit()
}

func statusIcon(s stepStatus) string {
	switch s {
	case stepFail:
		return "❌"
	case stepSkip:
		return "⊘"
	default:
		return "✅"
	}
}

// writeGitHubStepSummary appends a markdown table for this scenario to the file
// in $GITHUB_STEP_SUMMARY, which GitHub renders on the workflow run page. No-op
// outside Actions.
func (r *runner) writeGitHubStepSummary() {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	pass, fail, skip := r.stepCounts()
	overall := "✅ PASS"
	if fail > 0 {
		overall = "❌ FAIL"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## e2e · %s · %s\n\n", r.cfg.scenario, overall)
	fmt.Fprintf(&b, "%d passed · %d failed · %d skipped · cluster `%s`\n\n", pass, fail, skip, r.cfg.clusterName)
	b.WriteString("| # | step | result | time | description |\n|--:|------|:--:|--:|-------------|\n")
	for i, s := range r.steps {
		desc := s.desc
		if s.status == stepFail && s.errMsg != "" {
			desc += "<br>**" + mdEscape(truncateOneLine(s.errMsg, 240)) + "**"
		}
		fmt.Fprintf(&b, "| %d | `%s` | %s | %s | %s |\n", i+1, s.name, statusIcon(s.status), s.dur.Round(time.Second), desc)
	}
	b.WriteString("\n")
	appendFile(path, b.String())
}

// JUnit XML — uploaded with the diagnostics artifact so a Tests-tab reporter
// action (or any JUnit consumer) can render per-step results.
type junitSuite struct {
	XMLName  xml.Name    `xml:"testsuite"`
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Time     float64     `xml:"time,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Time      float64       `xml:"time,attr"`
	Failure   *junitMessage `xml:"failure,omitempty"`
	Skipped   *junitMessage `xml:"skipped,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
}

func (r *runner) writeJUnit() {
	dir := os.Getenv("DIAG_DIR")
	if dir == "" {
		dir = "e2e-diagnostics"
	}
	pass, fail, skip := r.stepCounts()
	_ = pass
	suite := junitSuite{Name: "magnum-e2e." + r.cfg.scenario, Tests: len(r.steps), Failures: fail, Skipped: skip}
	for _, s := range r.steps {
		c := junitCase{Name: s.name, ClassName: r.cfg.scenario, Time: s.dur.Seconds()}
		switch s.status {
		case stepFail:
			c.Failure = &junitMessage{Message: truncateOneLine(s.errMsg, 500)}
		case stepSkip:
			c.Skipped = &junitMessage{Message: "skipped after an earlier failure"}
		}
		suite.Time += s.dur.Seconds()
		suite.Cases = append(suite.Cases, c)
	}
	out, err := xml.MarshalIndent(suite, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	fn := filepath.Join(dir, "junit-"+sanitizeFilename(r.cfg.scenario)+".xml")
	if werr := os.WriteFile(fn, append([]byte(xml.Header), out...), 0o644); werr == nil {
		r.log("JUnit report written: %s", fn)
	}
}

func appendFile(path, content string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(content)
}

func truncateOneLine(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

func mdEscape(s string) string {
	return strings.NewReplacer("|", "\\|", "<", "&lt;", ">", "&gt;").Replace(s)
}

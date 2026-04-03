package result

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type Result struct {
	Status           string            `json:"status"`
	Step             string            `json:"step"`
	Summary          string            `json:"summary"`
	Reason           string            `json:"reason,omitempty"`
	Changed          []string          `json:"changed,omitempty"`
	Operations       []host.Change     `json:"operations,omitempty"`
	Warnings         []string          `json:"warnings,omitempty"`
	ErrorCode        string            `json:"errorCode,omitempty"`
	Details          map[string]string `json:"details,omitempty"`
	DeployStatusCode int               `json:"deploy_status_code"`
	DeployStdout     string            `json:"deploy_stdout,omitempty"`
	DeployStderr     string            `json:"deploy_stderr,omitempty"`
}

func Write(path string, result Result) error {
	data, err := json.MarshalIndent(Normalize(result), "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func Normalize(result Result) Result {
	if result.Reason == "" {
		result.Reason = result.Summary
	}

	if result.Status == "failed" {
		if result.DeployStatusCode == 0 {
			result.DeployStatusCode = 1
		}
		if result.DeployStderr == "" {
			result.DeployStderr = renderSignalText(result, true)
		}
		return result
	}

	if result.DeployStdout == "" {
		result.DeployStdout = renderSignalText(result, false)
	}

	return result
}

func renderSignalText(result Result, failed bool) string {
	lines := make([]string, 0, 8)
	if result.Summary != "" {
		lines = append(lines, result.Summary)
	}
	if result.Step != "" {
		lines = append(lines, fmt.Sprintf("step=%s", result.Step))
	}
	if failed && result.Reason != "" && result.Reason != result.Summary {
		lines = append(lines, fmt.Sprintf("reason=%s", result.Reason))
	}
	if result.ErrorCode != "" {
		lines = append(lines, fmt.Sprintf("errorCode=%s", result.ErrorCode))
	}
	for _, warning := range result.Warnings {
		lines = append(lines, fmt.Sprintf("warning=%s", warning))
	}

	if len(result.Details) > 0 {
		keys := make([]string, 0, len(result.Details))
		for key := range result.Details {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			lines = append(lines, fmt.Sprintf("%s=%s", key, result.Details[key]))
		}
	}

	return strings.Join(lines, "\n")
}

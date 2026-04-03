package journal

import (
	"encoding/json"
	"errors"
	"os"
	"syscall"
	"time"
)

type RunState struct {
	Status      string   `json:"status"`
	Mode        string   `json:"mode"`
	Instance    string   `json:"instance"`
	Role        string   `json:"role"`
	Operation   string   `json:"operation"`
	PID         int      `json:"pid"`
	StartedAt   string   `json:"startedAt,omitempty"`
	CompletedAt string   `json:"completedAt,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Phases      []string `json:"phases,omitempty"`
}

func Load(path string) (RunState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return RunState{}, nil
	}
	if err != nil {
		return RunState{}, err
	}

	var state RunState
	if err := json.Unmarshal(data, &state); err != nil {
		return RunState{}, err
	}
	return state, nil
}

func Write(path string, state RunState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func RecoverInterrupted(path string) (RunState, bool, error) {
	state, err := Load(path)
	if err != nil {
		return RunState{}, false, err
	}

	if state.Status != "running" || state.PID <= 0 {
		return state, false, nil
	}

	if processExists(state.PID) {
		return state, false, nil
	}

	state.Status = "interrupted"
	state.Summary = "previous reconcile run appears interrupted"
	state.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	state.PID = 0
	if err := Write(path, state); err != nil {
		return RunState{}, false, err
	}
	return state, true, nil
}

func MarkRunning(path, mode, instance, role, operation string, phases []string) error {
	return Write(path, RunState{
		Status:    "running",
		Mode:      mode,
		Instance:  instance,
		Role:      role,
		Operation: operation,
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Phases:    phases,
	})
}

func MarkCompleted(path, mode, summary string) error {
	state, err := Load(path)
	if err != nil {
		return err
	}
	state.Status = "completed"
	state.Mode = mode
	state.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	state.Summary = summary
	state.PID = 0
	return Write(path, state)
}

func MarkFailed(path, mode, summary string) error {
	state, err := Load(path)
	if err != nil {
		return err
	}
	state.Status = "failed"
	state.Mode = mode
	state.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	state.Summary = summary
	state.PID = 0
	return Write(path, state)
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

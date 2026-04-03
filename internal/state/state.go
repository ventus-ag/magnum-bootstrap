package state

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

type State struct {
	LastAttemptedGeneration         string   `json:"lastAttemptedGeneration,omitempty"`
	LastSuccessfulGeneration        string   `json:"lastSuccessfulGeneration,omitempty"`
	LastAttemptedReconcilerVersion  string   `json:"lastAttemptedReconcilerVersion,omitempty"`
	LastSuccessfulReconcilerVersion string   `json:"lastSuccessfulReconcilerVersion,omitempty"`
	LastKubeTag                     string   `json:"lastKubeTag,omitempty"`
	LastCARotationID                string   `json:"lastCaRotationId,omitempty"`
	LastRole                        string   `json:"lastRole,omitempty"`
	LastOperation                   string   `json:"lastOperation,omitempty"`
	LastInputChecksum               string   `json:"lastInputChecksum,omitempty"`
	PulumiProject                   string   `json:"pulumiProject,omitempty"`
	PulumiStack                     string   `json:"pulumiStack,omitempty"`
	PlannedPhases                   []string `json:"plannedPhases,omitempty"`
	UpdatedAt                       string   `json:"updatedAt,omitempty"`
}

func Load(path string) (State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, err
	}
	return state, nil
}

func Write(path string, state State) error {
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	client "example.com/swe-precheck-client"
)

// traeExecutionObservation is deliberately limited to facts emitted by the
// local Trae log counter. The session id is kept in memory only and is never
// returned by the workbench API.
type traeExecutionObservation struct {
	EffectiveRoundCount int  `json:"effective_round_count"`
	SessionCompleted    bool `json:"session_completed"`
}

// reconcileDetectedTrae advances a manually prepared task only when the real
// Trae workspace session has produced at least one plan item. This covers the
// common flow where the user pasted the prompt directly in Trae and never
// clicked the workbench's optional start action.
func (w *workbenchServer) reconcileDetectedTrae(ctx context.Context, path string) {
	state, err := client.NewWorkflowStore(path).Load(ctx)
	if err != nil || state.Status != client.WorkflowPrecheckPassed || state.Trae.ExecutionMode != "manual" || state.Workspace == "" {
		return
	}
	tool, err := workbenchTool(w.root, "count_trae_interactions.py")
	if err != nil {
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	output, err := exec.CommandContext(checkCtx, "python3", tool, "--workspace", state.Workspace).Output()
	if err != nil {
		return
	}
	var observation traeExecutionObservation
	if json.Unmarshal(output, &observation) != nil || observation.EffectiveRoundCount <= 0 || observation.SessionCompleted {
		return
	}
	store := client.NewWorkflowStore(path)
	_, _ = store.Update(ctx, state.Revision, client.WorkflowAudit{
		Action:  "trae_detected",
		Outcome: "success",
		Method:  "local_log",
	}, func(s *client.WorkflowState) error {
		if s.Status != client.WorkflowPrecheckPassed || s.Trae.ExecutionMode != "manual" {
			return nil
		}
		s.Trae.ExecutionStartedAt = time.Now().UTC().Format(time.RFC3339Nano)
		s.Status = client.WorkflowTraeRunning
		return nil
	})
}

func hasManualExecutionMode(state client.WorkflowState) bool {
	return state.Status == client.WorkflowPrecheckPassed && strings.EqualFold(state.Trae.ExecutionMode, "manual")
}

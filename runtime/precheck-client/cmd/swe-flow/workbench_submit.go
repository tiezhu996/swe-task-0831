package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	client "example.com/swe-precheck-client"
)

type workbenchApproval struct {
	TaskPathSHA256 string `json:"task_path_sha256"`
	PlanSHA256     string `json:"plan_sha256"`
	PromptSHA256   string `json:"prompt_sha256"`
	SessionSHA256  string `json:"session_sha256"`
	TraceSHA256    string `json:"trace_sha256"`
	ShotSHA256     string `json:"shot_sha256"`
	SchemaSHA256   string `json:"schema_sha256"`
	AuditSHA256    string `json:"audit_sha256"`
	PrecheckID     int    `json:"precheck_id"`
	AuthorizedAt   string `json:"authorized_at"`
}

type workbenchReview struct {
	SchemaVersion          int    `json:"schema_version"`
	PlanSHA256             string `json:"plan_sha256"`
	PromptSHA256           string `json:"prompt_sha256"`
	TraceSHA256            string `json:"trace_sha256"`
	SessionLogReviewed     bool   `json:"session_log_reviewed"`
	OneHumanPrompt         bool   `json:"one_human_prompt"`
	SourceDiffReviewed     bool   `json:"source_diff_reviewed"`
	RubricEvidenceReviewed bool   `json:"rubric_evidence_reviewed"`
	Notes                  string `json:"notes"`
}

func buildWorkbenchApproval(path string, state client.WorkflowState) (workbenchApproval, error) {
	if state.Status != client.WorkflowDryRunPassed || state.Submission.ID != 0 || (state.PendingOperation != nil && (state.PendingOperation.Outcome == "started" || state.PendingOperation.Outcome == "uncertain")) || state.PlanPath == "" || !samePlanHash(state) {
		return workbenchApproval{}, fmt.Errorf("task has not passed the current dry run")
	}
	plan, err := client.ReadSubmissionPlan(state.PlanPath)
	if err != nil {
		return workbenchApproval{}, err
	}
	tracePath, shotPath := plan.Attachments["x_trae_trace"], plan.Attachments["x_artifact_shot"]
	if tracePath == "" || shotPath == "" {
		return workbenchApproval{}, fmt.Errorf("required evidence missing")
	}
	traceHash, err := hashFile(tracePath)
	if err != nil {
		return workbenchApproval{}, err
	}
	shotHash, err := hashFile(shotPath)
	if err != nil {
		return workbenchApproval{}, err
	}
	auditHash, err := hashFile(state.Submission.AuditPath)
	if err != nil || auditHash != state.Submission.AuditSHA256 {
		return workbenchApproval{}, fmt.Errorf("dry-run report changed")
	}
	if state.PlanSHA256 == "" || state.Precheck.PromptSHA256 == "" || state.Trae.SessionTokenSHA256 == "" || state.Submission.SchemaSHA256 == "" {
		return workbenchApproval{}, fmt.Errorf("task bindings incomplete")
	}
	return workbenchApproval{
		TaskPathSHA256: planPathHash(path), PlanSHA256: state.PlanSHA256,
		PromptSHA256: state.Precheck.PromptSHA256, SessionSHA256: state.Trae.SessionTokenSHA256,
		TraceSHA256: traceHash, ShotSHA256: shotHash, SchemaSHA256: state.Submission.SchemaSHA256,
		AuditSHA256: auditHash, PrecheckID: state.Precheck.ItemID,
		AuthorizedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

func validateWorkbenchApproval(path string, state client.WorkflowState) (workbenchApproval, error) {
	var saved workbenchApproval
	if client.ReadLocalJSON(filepath.Join(filepath.Dir(path), "workbench-approval.json"), &saved) != nil {
		return saved, fmt.Errorf("task-specific authorization missing")
	}
	current, err := buildWorkbenchApproval(path, state)
	if err != nil {
		return saved, err
	}
	if saved.TaskPathSHA256 != current.TaskPathSHA256 || saved.PlanSHA256 != current.PlanSHA256 || saved.PromptSHA256 != current.PromptSHA256 || saved.SessionSHA256 != current.SessionSHA256 || saved.TraceSHA256 != current.TraceSHA256 || saved.ShotSHA256 != current.ShotSHA256 || saved.SchemaSHA256 != current.SchemaSHA256 || saved.AuditSHA256 != current.AuditSHA256 || saved.PrecheckID != current.PrecheckID || saved.AuthorizedAt == "" {
		return saved, fmt.Errorf("authorization is stale")
	}
	return saved, nil
}

func (w *workbenchServer) authorizeTask(out http.ResponseWriter, r *http.Request) {
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	var input struct {
		Confirm bool `json:"confirm"`
	}
	if decodeWorkbenchJSON(r, &input) != nil || !input.Confirm {
		workbenchError(out, 400, "需明确授权当前任务提交")
		return
	}
	if err := w.verifyTrace(r.Context(), state); err != nil {
		workbenchError(out, 409, "轨迹、冻结题面或来源确认未通过复核")
		return
	}
	approval, err := buildWorkbenchApproval(path, state)
	if err != nil {
		workbenchError(out, 409, "需先完成材料审核和 dry-run")
		return
	}
	if client.SaveLocalJSON(filepath.Join(filepath.Dir(path), "workbench-approval.json"), approval) != nil {
		workbenchError(out, 500, "授权记录保存失败")
		return
	}
	writeWorkbenchJSON(out, 200, map[string]any{"authorized": true, "plan_sha256": approval.PlanSHA256, "authorized_at": approval.AuthorizedAt})
}

func (w *workbenchServer) handoff(out http.ResponseWriter, r *http.Request) {
	path, state, err := w.loadTask(r.Context(), r.PathValue("key"))
	if err != nil {
		workbenchError(out, 409, "任务审计链不可用")
		return
	}
	approval, approvalErr := validateWorkbenchApproval(path, state)
	root := filepath.Dir(path)
	traceRoot := traceArchiveDirectory(state, path)
	result := map[string]any{
		"task_id": state.TaskID, "status": state.Status, "state_path": path,
		"workspace": state.Workspace, "baseline": state.Project.BaselineCommit,
		"prompt_path": filepath.Join(root, "PROMPT.md"), "trace_path": filepath.Join(traceRoot, "trae-export-sanitized.md"),
		"trace_receipt_path":     filepath.Join(traceRoot, "trae-export-sanitized.receipt.json"),
		"trace_directory":        traceDropDirectory(path),
		"terminal_manifest_path": filepath.Join(root, "native-evidence", "verification-manifest.json"),
		"schema_path":            state.Submission.SchemaPath, "review_path": filepath.Join(root, "workbench-review.json"),
		"precheck_id": state.Precheck.ItemID, "prompt_sha256": state.Precheck.PromptSHA256,
		"session_sha256": state.Trae.SessionTokenSHA256, "plan_sha256": state.PlanSHA256,
		"next_action":         actionForState(state, map[string]string{}).Action,
		"authorized":          approvalErr == nil,
		"execution_mode":      state.Trae.ExecutionMode,
		"codex_text":          manualCodexHandoffMode(path, state, r.URL.Query().Get("submit") == "1"),
		"submit_after_review": r.URL.Query().Get("submit") == "1",
	}
	gate := evaluateReviewGate(path, state)
	result["review_ready"] = gate.Ready
	result["review_reason"] = gate.Reason
	if gate.Template != nil {
		result["review_template"] = *gate.Template
	} else if approvalErr == nil {
		result["review_template"] = workbenchReview{SchemaVersion: 1, PlanSHA256: approval.PlanSHA256, PromptSHA256: approval.PromptSHA256, TraceSHA256: approval.TraceSHA256}
	}
	writeWorkbenchJSON(out, 200, result)
}

func validateWorkbenchReview(path string, approval workbenchApproval, reviewPath string) error {
	var review workbenchReview
	if client.ReadLocalJSON(reviewPath, &review) != nil {
		return fmt.Errorf("Codex review record missing")
	}
	if review.SchemaVersion != 1 || review.PlanSHA256 != approval.PlanSHA256 || review.PromptSHA256 != approval.PromptSHA256 || review.TraceSHA256 != approval.TraceSHA256 || !review.SessionLogReviewed || !review.OneHumanPrompt || !review.SourceDiffReviewed || !review.RubricEvidenceReviewed || review.Notes == "" {
		return fmt.Errorf("Codex review is incomplete or stale")
	}
	if filepath.Dir(reviewPath) != filepath.Dir(path) {
		return fmt.Errorf("review record must belong to this task")
	}
	return nil
}

func runWorkbenchSubmit(args []string) error {
	flags := flag.NewFlagSet("workbench-submit", flag.ContinueOnError)
	statePath := flags.String("state", "", "audited task-state.json")
	reviewPath := flags.String("review", "", "Codex review JSON bound to current evidence")
	terminalPath := flags.String("terminal-evidence", "", "native terminal verification manifest")
	tracePath := flags.String("trace-receipt", "", "Trae trace receipt")
	toolsPath := flags.String("tools", os.Getenv("SWE_FLOW_TOOLS"), "evidence tool directory")
	execute := flags.Bool("execute", false, "explicitly create the platform submission")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*execute || *statePath == "" {
		return fmt.Errorf("workbench-submit requires -state and -execute")
	}
	absoluteState, err := filepath.Abs(*statePath)
	if err != nil {
		return err
	}
	root := filepath.Dir(absoluteState)
	if *reviewPath == "" {
		*reviewPath = filepath.Join(root, "workbench-review.json")
	}
	if *terminalPath == "" {
		*terminalPath = filepath.Join(root, "native-evidence", "verification-manifest.json")
	}
	if *toolsPath != "" {
		_ = os.Setenv("SWE_FLOW_TOOLS", *toolsPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	store := client.NewWorkflowStore(absoluteState)
	state, err := store.Load(ctx)
	if err != nil {
		return fmt.Errorf("task audit chain unavailable: %w", err)
	}
	approval, err := validateWorkbenchApproval(absoluteState, state)
	if *tracePath == "" {
		*tracePath = filepath.Join(traceArchiveDirectory(state), "trae-export-sanitized.receipt.json")
	}
	if err != nil {
		return err
	}
	if err := validateWorkbenchReview(absoluteState, approval, *reviewPath); err != nil {
		return err
	}
	w := &workbenchServer{root: root}
	if err := w.verifyTrace(ctx, state); err != nil {
		return err
	}
	common := []string{"-state", absoluteState, "-terminal-evidence", *terminalPath, "-trace-receipt", *tracePath, "-tools", *toolsPath}
	if err := runOperation("submit", append(append([]string{}, common...), "-execute")); err != nil {
		return err
	}
	return runOperation("submission-status", []string{"-state", absoluteState})
}

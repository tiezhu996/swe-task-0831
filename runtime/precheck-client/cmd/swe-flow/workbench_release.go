package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	client "example.com/swe-precheck-client"
)

// The release gate splits submission into two steps. Codex finishes the
// review and leaves workbench-review.json bound to the current materials; the
// task then stays local as "审核完成·未提交" until the user clicks submit here.

const submitJobTimeout = 12 * time.Minute

// platformGateTTL bounds how stale the displayed open/closed state may be.
// It is display only: the submit path always reads the live schema again.
const platformGateTTL = 3 * time.Minute

type platformGate struct {
	Known     bool   `json:"known"`
	Open      bool   `json:"open"`
	Limited   bool   `json:"limited"`
	Frozen    bool   `json:"frozen"`
	Error     string `json:"error,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
	checked   time.Time
}

func (w *workbenchServer) platformGateStatus(ctx context.Context, fresh bool) platformGate {
	w.mu.Lock()
	cached := w.gate
	w.mu.Unlock()
	if !fresh && !cached.checked.IsZero() && time.Since(cached.checked) < platformGateTTL {
		return cached
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	gate := platformGate{checked: time.Now()}
	gate.CheckedAt = gate.checked.UTC().Format(time.RFC3339)
	platform, err := w.newClient(ctx)
	if err == nil {
		var schema client.FormSchema
		if schema, err = platform.SubmissionFormSchema(ctx); err == nil {
			gate.Known, gate.Limited, gate.Frozen = true, schema.Limited, schema.Frozen
			gate.Open = !schema.Limited && !schema.Frozen
		}
	}
	if err != nil {
		gate.Error = "平台提交状态读取失败"
		if cached.Known {
			// Keep the last real answer visible, marked with the failed refresh.
			cached.Error, cached.checked = gate.Error, gate.checked
			gate = cached
		}
	}
	w.mu.Lock()
	w.gate = gate
	w.mu.Unlock()
	return gate
}

func (w *workbenchServer) platformStatus(out http.ResponseWriter, r *http.Request) {
	writeWorkbenchJSON(out, 200, w.platformGateStatus(r.Context(), r.URL.Query().Get("refresh") == "1"))
}

func platformClosedMessage(gate platformGate) string {
	if gate.Limited {
		return "平台本周期提交额度已满，额度开放后再提交；本地审核结果保留，无需重新验收"
	}
	return "平台当前暂停提交，开放后再提交；本地审核结果保留"
}

type workbenchSubmitJob struct {
	State            string `json:"state"` // running, succeeded, failed, uncertain
	Phase            string `json:"phase,omitempty"`
	Message          string `json:"message,omitempty"`
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at,omitempty"`
	SubmissionID     int    `json:"submission_id,omitempty"`
	SubmissionStatus string `json:"submission_status,omitempty"`
	PlanSHA256       string `json:"plan_sha256,omitempty"`
}

type reviewGate struct {
	Ready    bool             `json:"ready"`
	Reason   string           `json:"reason,omitempty"`
	Review   *workbenchReview `json:"review,omitempty"`
	Template *workbenchReview `json:"template,omitempty"`
	Path     string           `json:"path"`
}

func submitResultPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "workbench-submit-result.json")
}

// evaluateReviewGate reports whether Codex has finished a review that still
// matches the dry-run audited materials. It never authorizes anything.
func evaluateReviewGate(path string, state client.WorkflowState) reviewGate {
	gate := reviewGate{Path: filepath.Join(filepath.Dir(path), "workbench-review.json")}
	if state.Submission.ID > 0 {
		gate.Reason = "已有平台提交，只查询回执"
		return gate
	}
	if state.Status == client.WorkflowLocalOnly {
		// A task can be dropped by hand before it ever ran, so the reason must not
		// claim the collection policy decided it.
		if state.Trae.EffectiveRounds == nil {
			gate.Reason = "已标记为废弃：本题不继续执行，也不提交平台，仅留存本地"
		} else {
			gate.Reason = fmt.Sprintf("已标记为废弃（有效轮数 %d）：不提交平台，仅留存本地", *state.Trae.EffectiveRounds)
		}
		return gate
	}
	if state.Status != client.WorkflowDryRunPassed {
		gate.Reason = "dry-run 尚未通过"
		return gate
	}
	candidate, err := buildWorkbenchApproval(path, state)
	if err != nil {
		gate.Reason = "dry-run 报告或附件已变化，需要 Codex 重新审核"
		return gate
	}
	gate.Template = &workbenchReview{SchemaVersion: 1, PlanSHA256: candidate.PlanSHA256, PromptSHA256: candidate.PromptSHA256, TraceSHA256: candidate.TraceSHA256}
	var review workbenchReview
	if _, statErr := os.Stat(gate.Path); statErr != nil {
		gate.Reason = "等待 Codex 写入审核记录"
		return gate
	}
	if client.ReadLocalJSON(gate.Path, &review) != nil {
		gate.Reason = "审核记录不可读"
		return gate
	}
	gate.Review = &review
	if err := validateWorkbenchReview(path, candidate, gate.Path); err != nil {
		gate.Reason = "审核记录与当前材料不一致或未逐项确认，需要 Codex 重新复核"
		return gate
	}
	gate.Ready = true
	return gate
}

func (w *workbenchServer) submitJob(key, statePath string) *workbenchSubmitJob {
	w.mu.Lock()
	job, ok := w.submitJobs[key]
	w.mu.Unlock()
	if ok {
		copy := *job
		return &copy
	}
	var saved workbenchSubmitJob
	if client.ReadLocalJSON(submitResultPath(statePath), &saved) == nil && saved.StartedAt != "" {
		if saved.State == "running" {
			// The serving process died mid-flight; only the platform receipt
			// can settle what happened.
			saved.State, saved.Message = "uncertain", "工作台在提交过程中重启，结果需按平台回执核对"
		}
		return &saved
	}
	return nil
}

func (w *workbenchServer) setSubmitJob(key, statePath string, job workbenchSubmitJob) {
	w.mu.Lock()
	if w.submitJobs == nil {
		w.submitJobs = map[string]*workbenchSubmitJob{}
	}
	copy := job
	w.submitJobs[key] = &copy
	w.mu.Unlock()
	_ = client.SaveLocalJSON(submitResultPath(statePath), job)
}

func (w *workbenchServer) submitStatus(out http.ResponseWriter, r *http.Request) {
	path, state, err := w.loadTask(r.Context(), r.PathValue("key"))
	if err != nil {
		workbenchError(out, 409, "任务审计链不可用或任务不存在")
		return
	}
	gate := evaluateReviewGate(path, state)
	writeWorkbenchJSON(out, 200, map[string]any{
		"gate": gate, "job": w.submitJob(r.PathValue("key"), path),
		"plan_sha256": state.PlanSHA256, "prompt_sha256": state.Precheck.PromptSHA256,
		"session_sha256": state.Trae.SessionTokenSHA256, "audit_sha256": state.Submission.AuditSHA256,
		"submission_id": state.Submission.ID, "submission_status": state.Submission.Status, "status": state.Status,
		"platform": w.platformGateStatus(r.Context(), false),
	})
}

func (w *workbenchServer) submitTask(out http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	var input struct {
		Confirm    bool   `json:"confirm"`
		PlanSHA256 string `json:"plan_sha256"`
	}
	if decodeWorkbenchJSON(r, &input) != nil || !input.Confirm {
		workbenchError(out, 400, "需要明确确认提交当前任务")
		return
	}
	if input.PlanSHA256 == "" || input.PlanSHA256 != state.PlanSHA256 {
		workbenchError(out, 409, "页面展示的材料已过期，请刷新后再提交")
		return
	}
	if reason, blocked := submissionBlockedLocally(state); blocked {
		workbenchError(out, 409, reason)
		return
	}
	w.mu.Lock()
	if job, running := w.submitJobs[key]; running && job.State == "running" {
		w.mu.Unlock()
		workbenchError(out, 409, "该任务正在提交，请等待结果")
		return
	}
	if w.submitJobs == nil {
		w.submitJobs = map[string]*workbenchSubmitJob{}
	}
	// Reserve the slot before any slow check so a double click cannot start two writes.
	w.submitJobs[key] = &workbenchSubmitJob{State: "running", Phase: "核对审核记录", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	w.mu.Unlock()
	release := func() {
		w.mu.Lock()
		delete(w.submitJobs, key)
		w.mu.Unlock()
	}
	gate := evaluateReviewGate(path, state)
	if !gate.Ready {
		release()
		workbenchError(out, 409, "尚未审核完成："+gate.Reason)
		return
	}
	if err := w.verifyTrace(r.Context(), state); err != nil {
		release()
		workbenchError(out, 409, "轨迹、冻结题面或来源确认未通过复核")
		return
	}
	approval, err := buildWorkbenchApproval(path, state)
	if err != nil {
		release()
		workbenchError(out, 409, "dry-run 材料已变化，需要重新审核")
		return
	}
	if client.SaveLocalJSON(filepath.Join(filepath.Dir(path), "workbench-approval.json"), approval) != nil {
		release()
		workbenchError(out, 500, "授权记录保存失败，未提交")
		return
	}
	if gate := w.platformGateStatus(r.Context(), true); gate.Known && !gate.Open {
		release()
		workbenchError(out, 409, platformClosedMessage(gate))
		return
	}
	tools := ""
	if tool, err := workbenchTool(w.root, "capture_trae_session.py"); err == nil {
		tools = filepath.Dir(tool)
	} else {
		release()
		workbenchError(out, 500, "Session 核验工具不可用，未提交")
		return
	}
	job := workbenchSubmitJob{State: "running", Phase: "正式审核、上传附件并创建提交", StartedAt: time.Now().UTC().Format(time.RFC3339), PlanSHA256: approval.PlanSHA256}
	w.setSubmitJob(key, path, job)
	go w.runSubmitJob(key, path, tools, job)
	writeWorkbenchJSON(out, 202, map[string]any{"job": job})
}

func (w *workbenchServer) runSubmitJob(key, path, tools string, job workbenchSubmitJob) {
	ctx, cancel := context.WithTimeout(context.Background(), submitJobTimeout)
	defer cancel()
	runErr := w.runSubmit(ctx, path, tools)
	job.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	job.Phase = ""
	state, loadErr := client.NewWorkflowStore(path).Load(context.Background())
	switch {
	case loadErr != nil:
		job.State, job.Message = "uncertain", "提交后任务状态不可读，需按平台回执核对"
	case state.Submission.ID > 0:
		job.State = "succeeded"
		job.SubmissionID, job.SubmissionStatus = state.Submission.ID, state.Submission.Status
		if runErr != nil {
			job.Message = "已创建平台提交，回执状态查询失败：" + runErr.Error()
		}
	case state.PendingOperation != nil && (state.PendingOperation.Outcome == "started" || state.PendingOperation.Outcome == "uncertain"):
		job.State, job.Message = "uncertain", "写请求结果未明，禁止重试；需用平台回执 reconcile"
		if runErr != nil {
			job.Message += "（" + runErr.Error() + "）"
		}
	case runErr != nil:
		job.State, job.Message = "failed", runErr.Error()
	default:
		job.State, job.Message = "failed", "提交流程结束但没有平台提交编号"
	}
	w.setSubmitJob(key, path, job)
}

func runWorkbenchSubmitProcess(ctx context.Context, statePath, tools string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, executable, "workbench-submit", "-state", statePath, "-tools", tools, "-execute")
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = nil, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("提交超时，结果需核对")
		}
		return fmt.Errorf("%s", lastLine(stderr.String(), "提交流程失败"))
	}
	return nil
}

func lastLine(output, fallback string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			if runes := []rune(line); len(runes) > 240 {
				line = string(runes[:240])
			}
			return line
		}
	}
	return fallback
}

func (w *workbenchServer) refreshSubmission(out http.ResponseWriter, r *http.Request) {
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	if state.Submission.ID <= 0 {
		workbenchError(out, 409, "还没有平台提交编号")
		return
	}
	if _, err := w.runFlow(r.Context(), "submission-status", "-state", path); err != nil {
		workbenchError(out, 409, "回执查询失败，请稍后重试")
		return
	}
	writeWorkbenchJSON(out, 200, w.taskView(r.Context(), path))
}

// workbenchStage maps the detailed state machine to the stage shown in the
// unified pipeline.
func workbenchStage(view workbenchTask, state client.WorkflowState, gate reviewGate, job *workbenchSubmitJob) string {
	// The task state is authoritative: a submit interrupted before its write
	// began leaves no pending operation and may be retried, while a started
	// write keeps the task blocked until reconciled.
	if view.IntegrityError || view.Pending {
		return "blocked"
	}
	if job != nil && job.State == "running" {
		return "submitting"
	}
	switch state.Status {
	case client.WorkflowSubmitting:
		return "blocked"
	case client.WorkflowSubmitted, client.WorkflowQCPending:
		return "submitted"
	case client.WorkflowQCRejected:
		return "rejected"
	case client.WorkflowQCAccepted, client.WorkflowClosed:
		return "done"
	case client.WorkflowLocalOnly:
		return "dropped"
	case client.WorkflowInitialized, client.WorkflowProjectSelected, client.WorkflowPrecheckDraft, client.WorkflowPrecheckSubmitted, client.WorkflowPrecheckRejected:
		return "precheck"
	}
	if state.Submission.ID > 0 {
		return "submitted"
	}
	if view.SessionSHA256 == "" || !view.Artifacts["x_trae_trace"] {
		return "execute"
	}
	if gate.Ready {
		return "ready"
	}
	return "review"
}

// submissionBlockedLocally refuses a run the collection policy will not take
// before any submission job starts. SubmitWorkflow enforces the same decision
// for the CLI path; this early check keeps the queue from showing a job that
// could never be created and tells the operator why the task stays local.
func submissionBlockedLocally(state client.WorkflowState) (string, bool) {
	if state.PlanPath == "" {
		return "", false
	}
	plan, err := client.ReadSubmissionPlan(state.PlanPath)
	if err != nil {
		return "", false
	}
	blocked, reason := client.LocalOnlyDecision(state, plan)
	return reason, blocked
}

// setLocalOnly marks a finished run as local-only, or reopens one so its
// materials can be corrected. The automatic decision covers runs whose
// evaluation is already recorded; this action covers the operator's override
// and the correction path the closed task has to keep.
func (w *workbenchServer) setLocalOnly(out http.ResponseWriter, r *http.Request) {
	w.evidenceMu.Lock()
	defer w.evidenceMu.Unlock()
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	var input struct {
		Local *bool `json:"local"`
	}
	if decodeWorkbenchJSON(r, &input) != nil || input.Local == nil {
		workbenchError(out, 400, "需要明确本地留存或重新打开的意图")
		return
	}
	var next client.WorkflowStatus
	if *input.Local {
		if !client.LocalOnlyMarkable(state.Status) {
			workbenchError(out, 409, "当前阶段不能标记为仅留存本地")
			return
		}
		next = client.WorkflowLocalOnly
	} else {
		if state.Status != client.WorkflowLocalOnly {
			workbenchError(out, 409, "只有已弃用的任务可以重新打开")
			return
		}
		// Resume where the task can actually continue: a captured session goes
		// back to the draft so its count can be corrected; a task that never ran
		// returns to the stage before its materials.
		switch {
		case state.Trae.SessionTokenSHA256 != "":
			next = client.WorkflowSessionCaptured
		case state.Precheck.ItemID > 0:
			next = client.WorkflowPrecheckPassed
		default:
			next = client.WorkflowProjectSelected
		}
	}
	store := client.NewWorkflowStore(path)
	if _, err := store.Update(r.Context(), state.Revision, client.WorkflowAudit{Action: "local_only_toggle", Outcome: "success"}, func(s *client.WorkflowState) error {
		s.Status = next
		return nil
	}); err != nil {
		workbenchError(out, 409, err.Error())
		return
	}
	writeWorkbenchJSON(out, 200, w.taskView(r.Context(), path))
}

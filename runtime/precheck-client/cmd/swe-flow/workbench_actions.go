package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	client "example.com/swe-precheck-client"
)

func (w *workbenchServer) actionTask(out http.ResponseWriter, r *http.Request) (string, client.WorkflowState, bool) {
	path, state, err := w.loadTask(r.Context(), r.PathValue("key"))
	if err != nil {
		workbenchError(out, 409, "任务审计链不可用或任务不存在")
		return "", state, false
	}
	if state.PendingOperation != nil && (state.PendingOperation.Outcome == "started" || state.PendingOperation.Outcome == "uncertain") {
		workbenchError(out, 409, "外部写请求结果未明，先核对平台回执")
		return "", state, false
	}
	return path, state, true
}

func (w *workbenchServer) flowAction(out http.ResponseWriter, r *http.Request, args ...string) bool {
	output, err := w.runFlow(r.Context(), args...)
	if err != nil {
		workbenchError(out, 409, "操作未完成，请查看任务状态和本地回执")
		return false
	}
	var result any
	if json.Unmarshal(output, &result) != nil {
		result = map[string]bool{"completed": true}
	}
	writeWorkbenchJSON(out, 200, result)
	return true
}

func (w *workbenchServer) submitPrecheck(out http.ResponseWriter, r *http.Request) {
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	if state.Status != client.WorkflowProjectSelected && state.Status != client.WorkflowPrecheckRejected {
		workbenchError(out, 409, "当前阶段不能提交预检")
		return
	}
	if !state.Verification.Preflight.Passed {
		workbenchError(out, 409, "本地预检尚未通过")
		return
	}
	var input struct {
		Confirm bool `json:"confirm"`
	}
	if decodeWorkbenchJSON(r, &input) != nil || !input.Confirm {
		workbenchError(out, 400, "需要确认提交预检")
		return
	}
	w.flowAction(out, r, "precheck-submit", "-state", path, "-prompt", filepath.Join(filepath.Dir(path), "PROMPT.md"), "-execute")
}

func (w *workbenchServer) refreshTaskPrecheck(out http.ResponseWriter, r *http.Request) {
	path, _, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	w.flowAction(out, r, "precheck-status", "-state", path)
}

func (w *workbenchServer) preparePlan(out http.ResponseWriter, r *http.Request) {
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	if err := w.prepareTaskPlan(r.Context(), path, state); err != nil {
		workbenchError(out, 409, err.Error())
		return
	}
	writeWorkbenchJSON(out, 200, w.taskView(r.Context(), path))
}

func (w *workbenchServer) prepareTaskPlan(ctx context.Context, path string, state client.WorkflowState) error {
	if state.Status != client.WorkflowPrecheckPassed && state.Status != client.WorkflowTraeRunning && state.Status != client.WorkflowSessionCaptured {
		return fmt.Errorf("当前阶段不能准备提交表单")
	}
	root := filepath.Dir(path)
	schemaPath := state.Submission.SchemaPath
	if schemaPath == "" {
		schemaPath = filepath.Join(root, "submission-schema.json")
		if _, err := os.Lstat(schemaPath); err == nil {
			return fmt.Errorf("已有未绑定的 schema 文件，先核查")
		}
		if _, err := w.runFlow(ctx, "schema-fetch", "-state", path, "-out", schemaPath); err != nil {
			return fmt.Errorf("读取平台表单失败")
		}
	}
	templatePath := filepath.Join(root, "submission-plan.template.json")
	if _, err := os.Lstat(templatePath); os.IsNotExist(err) {
		if err := client.SaveLocalJSON(templatePath, client.SubmissionPlan{Payload: map[string]any{}, Attachments: map[string]string{}}); err != nil {
			return fmt.Errorf("创建表单草稿失败")
		}
	}
	planPath := filepath.Join(root, "submission-plan.json")
	if _, err := os.Lstat(planPath); err == nil {
		return fmt.Errorf("提交计划已存在，不覆盖")
	}
	if _, err := w.runFlow(ctx, "prepare-plan", "-state", path, "-plan", templatePath, "-prompt", filepath.Join(root, "PROMPT.md"), "-schema", schemaPath, "-out", planPath); err != nil {
		return fmt.Errorf("生成提交计划失败")
	}
	if _, err := w.runFlow(ctx, "bind-plan", "-state", path, "-plan", planPath); err != nil {
		return fmt.Errorf("计划已保存，但绑定失败；请核查本地文件")
	}
	if err := w.reusePrecheckDesign(ctx, path); err != nil {
		return fmt.Errorf("计划已准备，但已有出题设计复用失败: %w", err)
	}
	draftPath := filepath.Join(root, "design-draft.json")
	if _, err := os.Lstat(draftPath); os.IsNotExist(err) {
		draft := client.DesignMaterials{SchemaVersion: 1, Origin: "historical_reconstruction", RepoURL: state.Project.RepoURL, BaselineCommit: state.Project.BaselineCommit, PromptSHA256: state.Precheck.PromptSHA256, Fields: map[string]string{}, SourceEvidence: []client.WorkflowArtifact{}}
		for _, key := range client.DesignFieldKeys {
			draft.Fields[key] = ""
		}
		if err := client.SaveLocalJSON(draftPath, draft); err != nil {
			return fmt.Errorf("计划已准备，但设计草稿模板保存失败")
		}
	}
	return nil
}

func (w *workbenchServer) traeStart(out http.ResponseWriter, r *http.Request) {
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	if state.Status != client.WorkflowPrecheckPassed {
		workbenchError(out, 409, "只有预检通过后才能记录 Trae 执行")
		return
	}
	if err := verifyPreparedSource(r.Context(), path, state); err != nil {
		workbenchError(out, 409, err.Error())
		return
	}
	var input struct {
		Model   string `json:"model"`
		Mode    string `json:"mode"`
		Confirm bool   `json:"confirm"`
	}
	if decodeWorkbenchJSON(r, &input) != nil || !input.Confirm || strings.TrimSpace(input.Model) == "" || strings.TrimSpace(input.Mode) != "manual" {
		workbenchError(out, 400, "需确认手动执行模式和 Trae 界面实际选中的模型")
		return
	}
	w.flowAction(out, r, "trae-start", "-state", path, "-model", input.Model, "-mode", "manual")
}

func workbenchTool(root, name string) (string, error) {
	var candidates []string
	if env := os.Getenv("SWE_FLOW_TOOLS"); env != "" {
		candidates = append(candidates, filepath.Join(env, name))
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "tools", name))
	}
	candidates = append(candidates, filepath.Join(root, "tools", name))
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, nil
		}
	}
	return "", fmt.Errorf("required evidence tool unavailable")
}

// runSessionCapture runs the capture tool once in the requested mode and
// returns the tool's own fixed-message reason on failure. The token travels on
// stdin, never in arguments, and nothing but that fixed reason is forwarded.
func runSessionCapture(ctx context.Context, tool, planPath, workspace, taskState, token string, historical bool) (string, error) {
	arguments := []string{tool, "--plan", planPath, "--workspace", workspace, "--task-state", taskState, "--stdin", "--update-rounds", "--apply"}
	if historical {
		arguments = append(arguments, "--historical")
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "python3", arguments...)
	cmd.Stdin = strings.NewReader(token)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		reason := sessionCaptureReason(stdout.String() + "\n" + stderr.String())
		return reason, fmt.Errorf("%s", reason)
	}
	return "", nil
}

// The capture tool prints its fixed, token-free reason to stdout as
// "session_capture=BLOCKED ..." and exits non-zero. Reading only stderr erased
// every actionable reason and left the web layer with one opaque sentence.
func sessionCaptureReason(output string) string {
	reason := ""
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "session_capture=BLOCKED ") {
			continue
		}
		if value := strings.TrimSpace(strings.TrimPrefix(line, "session_capture=BLOCKED ")); value != "" {
			reason = value
		}
	}
	if reason == "" {
		reason = "Session 与目标工作区或真实日志不匹配"
	}
	if runes := []rune(reason); len(runes) > 300 {
		reason = string(runes[:300])
	}
	return reason
}

func transientSessionCapture(reason string) bool {
	return strings.Contains(reason, "核验期间日志发生变化") ||
		strings.Contains(reason, "核验期间日志集合") ||
		strings.Contains(reason, "日志集合或工作区会话发生变化")
}

func (w *workbenchServer) captureSession(out http.ResponseWriter, r *http.Request) {
	w.evidenceMu.Lock()
	defer w.evidenceMu.Unlock()
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	if state.Status != client.WorkflowPrecheckPassed && state.Status != client.WorkflowTraeRunning && state.Status != client.WorkflowSessionCaptured {
		workbenchError(out, 409, "当前阶段不能采集 Session")
		return
	}
	var input struct {
		Token      string `json:"token"`
		Historical bool   `json:"historical"`
		SkipVerify bool   `json:"skip_verify"`
	}
	if decodeWorkbenchJSON(r, &input) != nil || len(input.Token) > 1024 || (input.Token != "" && client.ValidateTraeSessionToken(input.Token) != nil) {
		workbenchError(out, 400, "Session 原串格式不完整")
		return
	}
	state, err := w.prepareManualCapture(r.Context(), path, state)
	if err != nil {
		// A temporary skip only needs the token hash and the state transition.
		// If plan preparation already bound a plan before a separate design-reuse
		// warning, keep going instead of making the operator click twice.
		if !input.SkipVerify {
			workbenchError(out, 409, err.Error())
			return
		}
		reloaded, loadErr := client.NewWorkflowStore(path).Load(r.Context())
		if loadErr != nil || reloaded.PlanPath == "" {
			workbenchError(out, 409, err.Error())
			return
		}
		state = reloaded
	}
	if input.SkipVerify {
		// Temporary operator request: save the original string and its hash,
		// but do not match it against Trae logs. The state carries an explicit
		// flag so the UI and the Codex handoff cannot present it as verified.
		if state.PlanPath == "" {
			workbenchError(out, 409, "任务还没有提交计划，不能保存 Session")
			return
		}
		plan, planErr := client.ReadSubmissionPlan(state.PlanPath)
		if planErr != nil {
			workbenchError(out, 409, "提交计划不可读")
			return
		}
		token := input.Token
		if token == "" {
			token, _ = plan.Payload["trae_session_id"].(string)
		}
		if token == "" {
			workbenchError(out, 409, "请先粘贴完整 Session 原串")
			return
		}
		plan.Payload["trae_session_id"] = token
		if err := client.SaveLocalJSON(state.PlanPath, plan); err != nil {
			workbenchError(out, 409, "Session 原串保存失败")
			return
		}
		if _, err := w.runFlow(r.Context(), "bind-plan", "-state", path, "-plan", state.PlanPath); err != nil {
			workbenchError(out, 409, "Session 已保存，但计划绑定失败")
			return
		}
		state, err = client.NewWorkflowStore(path).Load(r.Context())
		if err != nil {
			workbenchError(out, 409, "Session 已保存，但任务状态不可读")
			return
		}
		store := client.NewWorkflowStore(path)
		if state.Status == client.WorkflowTraeRunning {
			state, err = store.Advance(r.Context(), state.Revision, client.WorkflowSessionCaptured, client.WorkflowAudit{Action: "session_captured", Outcome: "success"})
			if err != nil {
				workbenchError(out, 409, "Session 已保存，但状态推进失败")
				return
			}
		}
		if state.Status != client.WorkflowSessionCaptured {
			workbenchError(out, 409, "当前阶段不能保存 Session")
			return
		}
		if _, err := store.Update(r.Context(), state.Revision, client.WorkflowAudit{Action: "session_verification_skipped", Outcome: "success"}, func(s *client.WorkflowState) error {
			s.Trae.SessionTokenSHA256 = client.TextSHA256(token)
			s.Trae.SessionVerificationSkipped = true
			if rounds, ok := plan.Payload["effective_rounds"].(float64); ok {
				value := int(rounds)
				s.Trae.EffectiveRounds = &value
			}
			s.Status = client.WorkflowSessionVerified
			return nil
		}); err != nil {
			workbenchError(out, 409, "Session 原串已保存，但状态记录失败")
			return
		}
		writeWorkbenchJSON(out, 200, w.taskView(r.Context(), path))
		return
	}

	if input.Token == "" {
		// A first click can save the original string and still stop at a log
		// that kept moving. Re-verifying an already saved string must not force
		// the user to paste it again, or the task would be a dead end.
		token := ""
		if state.PlanPath != "" {
			if plan, planErr := client.ReadSubmissionPlan(state.PlanPath); planErr == nil {
				token, _ = plan.Payload["trae_session_id"].(string)
			}
		}
		if token == "" {
			workbenchError(out, 409, "尚无已保存的 Session 原串，请粘贴完整原串")
			return
		}
	} else {
		tool, err := workbenchTool(w.root, "capture_trae_session.py")
		if err != nil {
			workbenchError(out, 500, "Session 核验工具不可用")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		reason, captureErr := runSessionCapture(ctx, tool, state.PlanPath, state.Workspace, path, input.Token, input.Historical)
		if captureErr != nil && !input.Historical && transientSessionCapture(reason) {
			// Trae can still append one unrelated renderer line while the log is
			// being read. One short settle-and-retry avoids turning that into a
			// hard failure before falling back to the historical snapshot.
			select {
			case <-time.After(900 * time.Millisecond):
			case <-ctx.Done():
			}
			reason, captureErr = runSessionCapture(ctx, tool, state.PlanPath, state.Workspace, path, input.Token, false)
		}
		if captureErr != nil {
			if input.Historical {
				workbenchError(out, 409, reason)
				return
			}
			// Trae rotates the renderer log on every restart, so a finished run's
			// evidence is usually only in its per-session snapshot repository.
			// Retry there instead of failing the operator; the audit records
			// which source actually proved the session.
			historicalCtx, historicalCancel := context.WithTimeout(r.Context(), 3*time.Minute)
			defer historicalCancel()
			if historicalReason, historicalErr := runSessionCapture(historicalCtx, tool, state.PlanPath, state.Workspace, path, input.Token, true); historicalErr != nil {
				workbenchError(out, 409, historicalReason)
				return
			}
		}
	}
	if _, err := w.runFlow(r.Context(), "bind-plan", "-state", path, "-plan", state.PlanPath); err != nil {
		workbenchError(out, 409, "Session 已私密保存，但状态绑定失败")
		return
	}
	if _, err := w.runFlow(r.Context(), "session-verify", "-state", path); err != nil {
		workbenchError(out, 409, "Session 已保存，但核验未通过：证据不匹配或核验期间发生变化，请核对原串与执行记录后重试")
		return
	}
	writeWorkbenchJSON(out, 200, w.taskView(r.Context(), path))
}

type traceAttestation struct {
	Source        string `json:"source"`
	RawSHA256     string `json:"raw_sha256"`
	PromptSHA256  string `json:"prompt_sha256"`
	SessionSHA256 string `json:"session_sha256"`
	ConfirmedAt   string `json:"confirmed_at"`
}

func (w *workbenchServer) uploadTrace(out http.ResponseWriter, r *http.Request) {
	w.evidenceMu.Lock()
	defer w.evidenceMu.Unlock()
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	if state.Status != client.WorkflowSessionVerified {
		workbenchError(out, 409, "先完成 Session 日志核验")
		return
	}
	if state.PlanPath == "" {
		workbenchError(out, 409, "缺提交计划")
		return
	}
	r.Body = http.MaxBytesReader(out, r.Body, 32<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		workbenchError(out, 413, "轨迹文件超过 32 MiB 或格式不正确")
		return
	}
	defer r.MultipartForm.RemoveAll()
	if r.FormValue("attested") != "true" {
		workbenchError(out, 400, "需确认文件来自目标 Trae 会话的原生导出")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil || !strings.EqualFold(filepath.Ext(header.Filename), ".md") {
		workbenchError(out, 400, "请选择 Trae 原生 Markdown 文件")
		return
	}
	defer file.Close()
	root := filepath.Dir(path)
	target, err := os.CreateTemp(root, ".trae-upload-*.md")
	if err != nil {
		workbenchError(out, 500, "无法私密保存轨迹")
		return
	}
	defer os.Remove(target.Name())
	_, copyErr := io.Copy(target, io.LimitReader(file, 32<<20))
	closeErr := target.Close()
	if copyErr != nil || closeErr != nil {
		workbenchError(out, 500, "轨迹保存不完整")
		return
	}
	if err := w.archiveTrace(r.Context(), path, state, target.Name()); err != nil {
		workbenchError(out, 409, err.Error())
		return
	}
	writeWorkbenchJSON(out, 200, w.taskView(r.Context(), path))
}

func (w *workbenchServer) auditTask(out http.ResponseWriter, r *http.Request) {
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	var input struct {
		Stage string `json:"stage"`
	}
	if decodeWorkbenchJSON(r, &input) != nil {
		workbenchError(out, 400, "审核阶段无效")
		return
	}
	command := ""
	if input.Stage == "artifacts" && state.Status == client.WorkflowSessionVerified {
		command = "artifact-check"
	}
	if input.Stage == "dry-run" && (state.Status == client.WorkflowArtifactsVerified || state.Status == client.WorkflowDryRunPassed) {
		command = "dry-run"
	}
	if command == "" {
		workbenchError(out, 409, "当前任务阶段不能执行该审核")
		return
	}
	if err := w.verifyTrace(r.Context(), state); err != nil {
		workbenchError(out, 409, "轨迹、题面或来源确认复核失败")
		return
	}
	root := filepath.Dir(path)
	manifest := filepath.Join(root, "native-evidence", "verification-manifest.json")
	receipt := filepath.Join(traceArchiveDirectory(state), "trae-export-sanitized.receipt.json")
	args := []string{command, "-state", path, "-terminal-evidence", manifest, "-trace-receipt", receipt, "-schema", state.Submission.SchemaPath, "-online"}
	w.flowAction(out, r, args...)
}

func hashFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

func (w *workbenchServer) verifyTrace(ctx context.Context, state client.WorkflowState) error {
	if state.PlanPath == "" {
		return fmt.Errorf("missing plan")
	}
	root := traceArchiveDirectory(state)
	tool, err := workbenchTool(w.root, "collect_trae_export.py")
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "python3", tool, "--input", filepath.Join(root, "trae-export-original.private.md"), "--plan", state.PlanPath, "--out", filepath.Join(root, "trae-export-sanitized.md"), "--receipt", filepath.Join(root, "trae-export-sanitized.receipt.json"), "--verify-only")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if cmd.Run() != nil {
		return fmt.Errorf("trajectory no longer matches the frozen prompt")
	}
	var attestation traceAttestation
	if client.ReadLocalJSON(filepath.Join(root, "trae-source-attestation.json"), &attestation) != nil || attestation.Source != "human_confirmed_native_export" {
		return fmt.Errorf("native export source declaration missing")
	}
	rawHash, err := hashFile(filepath.Join(root, "trae-export-original.private.md"))
	if err != nil || rawHash != attestation.RawSHA256 || attestation.PromptSHA256 != state.Precheck.PromptSHA256 || attestation.SessionSHA256 != state.Trae.SessionTokenSHA256 {
		return fmt.Errorf("source declaration no longer matches the evidence")
	}
	return nil
}

func planPathHash(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])
}

func samePlanHash(state client.WorkflowState) bool {
	hash, err := hashFile(state.PlanPath)
	return err == nil && hash == state.PlanSHA256
}

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"reflect"
	"time"

	client "example.com/swe-precheck-client"
)

var protectedWorkbenchFields = map[string]bool{
	"requirement_prompt": true, "precheck_item_id": true, "repo_url": true,
	"commit_ref": true, "primary_language": true, "seed_model": true,
	"trae_session_id": true, "trae_session_id_2": true,
	"effective_rounds": true, "seed_rounds": true,
	"remarks": true, "remark": true, "notes": true, "note": true, "备注": true,
}

type workbenchFormField struct {
	client.FormField
	Value      any                    `json:"value,omitempty"`
	Configured bool                   `json:"configured"`
	Locked     bool                   `json:"locked"`
	Editable   bool                   `json:"editable"`
	Review     client.FieldReviewItem `json:"review"`
}

type workbenchFieldUnlock struct {
	Path, Field, PlanSHA256 string
	Expires                 time.Time
}

func editableWorkbenchField(field client.FormField) bool {
	return !field.Auto && !protectedWorkbenchFields[field.Key] && field.Type != "attachment" && field.Type != "file"
}

func editableWorkbenchState(state client.WorkflowState) bool {
	if state.PlanPath == "" || state.Submission.ID > 0 || state.PendingOperation != nil && (state.PendingOperation.Outcome == "started" || state.PendingOperation.Outcome == "uncertain") {
		return false
	}
	switch state.Status {
	case client.WorkflowPrecheckPassed, client.WorkflowTraeRunning, client.WorkflowSessionCaptured, client.WorkflowSessionVerified, client.WorkflowArtifactsVerified, client.WorkflowDryRunPassed:
		return true
	}
	return false
}

func (w *workbenchServer) liveSchema(r *http.Request) (client.FormSchema, error) {
	platform, err := w.newClient(r.Context())
	if err != nil {
		return client.FormSchema{}, err
	}
	return platform.SubmissionFormSchema(r.Context())
}

func (w *workbenchServer) getForm(out http.ResponseWriter, r *http.Request) {
	_, state, err := w.loadTask(r.Context(), r.PathValue("key"))
	if err != nil {
		workbenchError(out, 409, "任务审计链不可用")
		return
	}
	schema, err := w.liveSchema(r)
	if err != nil {
		workbenchError(out, 503, "平台表单规则读取失败")
		return
	}
	var plan client.SubmissionPlan
	if state.PlanPath != "" {
		plan, err = client.ReadSubmissionPlan(state.PlanPath)
		if err != nil {
			workbenchError(out, 409, "本地提交计划不可读")
			return
		}
	}
	fields := make([]workbenchFormField, 0, len(schema.Fields))
	report := client.ReviewSubmissionFields(plan, schema)
	reviews := map[string]client.FieldReviewItem{}
	for _, item := range report.Fields {
		reviews[item.Key] = item
	}
	for _, field := range schema.Fields {
		item := workbenchFormField{FormField: field, Locked: true, Editable: editableWorkbenchField(field)}
		item.Review = reviews[field.Key]
		for _, key := range client.DesignFieldKeys {
			if field.Key == key && state.Verification.DesignPath != "" {
				item.Editable = false
			}
		}
		if field.Type == "attachment" {
			item.Configured = plan.Attachments[field.Key] != ""
		}
		if field.Key != "trae_session_id" && field.Key != "trae_session_id_2" && field.Type != "attachment" && field.Type != "file" {
			item.Value = plan.Payload[field.Key]
			item.Configured = plan.Payload[field.Key] != nil && plan.Payload[field.Key] != ""
		}
		if protectedWorkbenchFields[field.Key] && field.Key != "trae_session_id" && field.Key != "trae_session_id_2" {
			item.Configured = plan.Payload[field.Key] != nil
		}
		if field.Key == "trae_session_id" {
			item.Configured = state.Trae.SessionTokenSHA256 != ""
		}
		fields = append(fields, item)
	}
	changed := state.Submission.SchemaSHA256 != "" && !client.FormSchemaMatches(state.Submission.SchemaSHA256, schema)
	writeWorkbenchJSON(out, 200, map[string]any{"fields": fields, "version": schema.Version, "frozen": schema.Frozen, "limited": schema.Limited, "schema_changed": changed, "plan_ready": state.PlanPath != "", "plan_sha256": state.PlanSHA256, "editable": editableWorkbenchState(state) && !changed && !schema.Frozen && !schema.Limited})
}

// A field grant belongs to one plan revision and expires. Confirming on the
// browser alone does not unlock arbitrary fields on the server.
func (w *workbenchServer) unlockFormField(out http.ResponseWriter, r *http.Request) {
	w.formMu.Lock()
	defer w.formMu.Unlock()
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	var input struct {
		Field      string `json:"field"`
		Confirm    bool   `json:"confirm"`
		PlanSHA256 string `json:"plan_sha256"`
	}
	if decodeWorkbenchJSON(r, &input) != nil || !input.Confirm {
		workbenchError(out, 400, "需要确认解锁当前字段")
		return
	}
	if !editableWorkbenchState(state) || input.PlanSHA256 != state.PlanSHA256 || !samePlanHash(state) {
		workbenchError(out, 409, "材料已变化或任务只读，请刷新后再编辑")
		return
	}
	schema, err := w.liveSchema(r)
	if err != nil || schema.Frozen || !client.FormSchemaMatches(state.Submission.SchemaSHA256, schema) {
		workbenchError(out, 409, "平台字段规则未核实或已变化，暂不能解锁")
		return
	}
	allowed := false
	for _, field := range schema.Fields {
		if field.Key == input.Field {
			allowed = editableWorkbenchField(field)
			break
		}
	}
	if !allowed {
		workbenchError(out, 400, "该字段由冻结题面或实际证据绑定，不能直接修改")
		return
	}
	for _, key := range client.DesignFieldKeys {
		if input.Field == key && state.Verification.DesignPath != "" {
			workbenchError(out, 409, "设计材料已冻结，不能按产物结果修改标准")
			return
		}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		workbenchError(out, 500, "无法解锁字段")
		return
	}
	token := hex.EncodeToString(secret)
	if w.fieldUnlocks == nil {
		w.fieldUnlocks = map[string]workbenchFieldUnlock{}
	}
	for key, grant := range w.fieldUnlocks {
		if time.Now().After(grant.Expires) {
			delete(w.fieldUnlocks, key)
		}
	}
	w.fieldUnlocks[token] = workbenchFieldUnlock{Path: path, Field: input.Field, PlanSHA256: state.PlanSHA256, Expires: time.Now().Add(15 * time.Minute)}
	writeWorkbenchJSON(out, 200, map[string]string{"field": input.Field, "unlock_token": token})
}

func validateWorkbenchValue(field client.FormField, value any) error {
	switch field.Type {
	case "text", "textarea", "url", "date", "select":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("field must be text")
		}
		if field.MaxLength > 0 && len([]rune(text)) > field.MaxLength {
			return fmt.Errorf("field too long")
		}
		if field.Type == "select" && text != "" {
			valid := false
			for _, option := range field.Options {
				if text == option {
					valid = true
					break
				}
			}
			if !valid {
				return fmt.Errorf("invalid option")
			}
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("field must be a number")
		}
	default:
		return fmt.Errorf("unsupported field type")
	}
	return nil
}

func (w *workbenchServer) putForm(out http.ResponseWriter, r *http.Request) {
	w.formMu.Lock()
	defer w.formMu.Unlock()
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	if state.PlanPath == "" || state.Submission.ID > 0 {
		workbenchError(out, 409, "当前任务没有可编辑的提交计划")
		return
	}
	var input struct {
		Values       map[string]any    `json:"values"`
		UnlockTokens map[string]string `json:"unlock_tokens"`
		PlanSHA256   string            `json:"plan_sha256"`
	}
	if decodeWorkbenchJSON(r, &input) != nil || len(input.Values) == 0 {
		workbenchError(out, 400, "未提交有效表单字段")
		return
	}
	schema, err := w.liveSchema(r)
	if err != nil {
		workbenchError(out, 503, "平台表单规则读取失败")
		return
	}
	if !client.FormSchemaMatches(state.Submission.SchemaSHA256, schema) {
		workbenchError(out, 409, "平台 schema 已变化，不能修改旧计划")
		return
	}
	if !editableWorkbenchState(state) || schema.Frozen || input.PlanSHA256 != state.PlanSHA256 || !samePlanHash(state) {
		workbenchError(out, 409, "材料已变化或任务只读，请刷新后重新确认字段")
		return
	}
	plan, err := client.ReadSubmissionPlan(state.PlanPath)
	if err != nil {
		workbenchError(out, 409, "提交计划不可读")
		return
	}
	fields := map[string]client.FormField{}
	for _, field := range schema.Fields {
		fields[field.Key] = field
	}
	if plan.Payload == nil {
		plan.Payload = map[string]any{}
	}
	for key, value := range input.Values {
		for _, designKey := range client.DesignFieldKeys {
			if key == designKey && state.Verification.DesignPath != "" {
				workbenchError(out, 409, "设计材料已冻结，不能直接修改")
				return
			}
		}
		field, exists := fields[key]
		if !exists || field.Auto || protectedWorkbenchFields[key] || field.Type == "attachment" || field.Type == "file" || validateWorkbenchValue(field, value) != nil {
			workbenchError(out, 400, "含不可编辑或不符合表单规则的字段")
			return
		}
		grant, exists := w.fieldUnlocks[input.UnlockTokens[key]]
		if !exists || grant.Path != path || grant.Field != key || grant.PlanSHA256 != state.PlanSHA256 || time.Now().After(grant.Expires) {
			workbenchError(out, 400, "字段仍锁定，请双击并确认后修改")
			return
		}
		plan.Payload[key] = value
	}
	oldPlan, _ := client.ReadSubmissionPlan(state.PlanPath)
	if reflect.DeepEqual(oldPlan.Payload, plan.Payload) {
		for _, token := range input.UnlockTokens {
			delete(w.fieldUnlocks, token)
		}
		writeWorkbenchJSON(out, 200, map[string]bool{"saved": true})
		return
	}
	shadow := state
	shadow.Status = client.WorkflowSessionVerified
	if _, err := client.PrepareWorkflowPlan(shadow, plan, schema, plan.ExecutionPrompt); err != nil {
		workbenchError(out, 409, "字段与当前任务绑定不一致")
		return
	}
	if state.Status == client.WorkflowDryRunPassed || state.Status == client.WorkflowArtifactsVerified {
		next := client.WorkflowArtifactsVerified
		if state.Status == client.WorkflowArtifactsVerified {
			next = client.WorkflowSessionVerified
		}
		store := client.NewWorkflowStore(path)
		state, err = store.Update(r.Context(), state.Revision, client.WorkflowAudit{Action: "workbench_form_reset", Outcome: "success"}, func(s *client.WorkflowState) error { s.Status = next; return nil })
		if err != nil {
			workbenchError(out, 409, "无法重置旧审核状态")
			return
		}
	}
	if state.Status != client.WorkflowPrecheckPassed && state.Status != client.WorkflowTraeRunning && state.Status != client.WorkflowSessionCaptured && state.Status != client.WorkflowSessionVerified && state.Status != client.WorkflowArtifactsVerified {
		workbenchError(out, 409, "当前阶段不能编辑提交表单")
		return
	}
	if err := client.SaveLocalJSON(state.PlanPath, plan); err != nil {
		workbenchError(out, 500, "保存提交计划失败")
		return
	}
	if _, err := w.runFlow(r.Context(), "bind-plan", "-state", path, "-plan", state.PlanPath); err != nil {
		workbenchError(out, 409, "表单已保存但状态绑定失败，请核查")
		return
	}
	for _, token := range input.UnlockTokens {
		delete(w.fieldUnlocks, token)
	}
	writeWorkbenchJSON(out, 200, map[string]bool{"saved": true})
}

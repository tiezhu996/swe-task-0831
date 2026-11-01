package precheckclient

import (
	"strings"
	"testing"
)

func workflowPlanFixture() (WorkflowState, SubmissionPlan, FormSchema, string) {
	prompt := "我们每天查询分日期导出的文件。遇到文件缺失时需明确失败，保持原有成功场景。"
	rounds := 45
	state := WorkflowState{
		Status:   WorkflowSessionVerified,
		Precheck: WorkflowPrecheck{ItemID: 2409, PromptSHA256: TextSHA256(prompt)},
		Project:  WorkflowProject{RepoURL: "https://github.com/example/repo", BaselineCommit: strings.Repeat("a", 40), Language: "Go"},
		Trae:     WorkflowTrae{Model: "Actual Model", EffectiveRounds: &rounds},
	}
	plan := SubmissionPlan{SchemaVersion: "old", Payload: map[string]any{"title": "批量失败处理", "trae_session_id": "", "effective_rounds": 0}, Attachments: map[string]string{"x_trae_trace": "", "x_artifact_shot": ""}}
	schema := FormSchema{Version: "current", Fields: []FormField{
		{Key: "title", Type: "text", Required: true},
		{Key: "precheck_item_id", Type: "precheck_ref", Required: true},
		{Key: "repo_url", Type: "url", Required: true},
		{Key: "commit_ref", Type: "text", Required: true},
		{Key: "primary_language", Type: "select", Options: []string{"Go", "Rust"}},
		{Key: "seed_model", Type: "select", Options: []string{"Actual Model"}},
		{Key: "effective_rounds", Type: "number"},
		{Key: "trae_session_id", Type: "text", Required: true},
		{Key: "x_trae_trace", Type: "attachment", Required: true},
		{Key: "x_artifact_shot", Type: "attachment", Required: true},
		{Key: "requirement_prompt", Type: "textarea", Auto: true},
	}}
	return state, plan, schema, prompt
}

func TestPrepareWorkflowPlanInjectsVerifiedBindingsAndKeepsDraft(t *testing.T) {
	state, template, schema, prompt := workflowPlanFixture()
	prepared, err := PrepareWorkflowPlan(state, template, schema, prompt)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.SchemaVersion != schema.Version || prepared.ExecutionPrompt != prompt || !strings.HasPrefix(prepared.ExecutionPrompt, "我们") {
		t.Fatal("prechecked prompt or current schema was changed incorrectly")
	}
	for key, expected := range map[string]any{"precheck_item_id": 2409, "repo_url": state.Project.RepoURL, "commit_ref": state.Project.BaselineCommit, "primary_language": "Go", "seed_model": "Actual Model", "effective_rounds": 45} {
		if prepared.Payload[key] != expected {
			t.Fatalf("workflow binding %q was not injected", key)
		}
	}
	if prepared.Payload["trae_session_id"] != "" || prepared.Attachments["x_trae_trace"] != "" || prepared.Attachments["x_artifact_shot"] != "" {
		t.Fatal("draft placeholders were not preserved")
	}
	if _, exists := template.Payload["precheck_item_id"]; exists || template.SchemaVersion != "old" {
		t.Fatal("caller template was mutated")
	}
	prepared.Attachments["x_trae_trace"] = "changed"
	if template.Attachments["x_trae_trace"] != "" {
		t.Fatal("attachment map aliases caller template")
	}
}

func TestPrepareWorkflowPlanAllowsMissingObservedModelAndRounds(t *testing.T) {
	state, template, schema, prompt := workflowPlanFixture()
	state.Status, state.Trae.Model, state.Trae.EffectiveRounds = WorkflowPrecheckPassed, "", nil
	prepared, err := PrepareWorkflowPlan(state, template, schema, prompt)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := prepared.Payload["seed_model"]; exists {
		t.Fatal("model was invented before Trae observation")
	}
	if prepared.Payload["effective_rounds"] != float64(0) {
		t.Fatal("unobserved round count was invented")
	}
}

func TestPrepareWorkflowPlanAllowsLimitedSubmissionSchema(t *testing.T) {
	state, template, schema, prompt := workflowPlanFixture()
	schema.Limited = true
	prepared, err := PrepareWorkflowPlan(state, template, schema, prompt)
	if err != nil {
		t.Fatalf("limited submission schema should still allow local draft preparation: %v", err)
	}
	if prepared.SchemaVersion != schema.Version || prepared.ExecutionPrompt != prompt {
		t.Fatal("local draft was not prepared from the verified prompt and schema")
	}
}

func TestPrepareWorkflowPlanAcceptsFrozenPromptBeyondDraftLimit(t *testing.T) {
	// The platform froze this text for the precheck and it is bound by exact
	// SHA-256, so the local 240-rune drafting rule must not block the draft.
	state, template, schema, _ := workflowPlanFixture()
	prompt := strings.Repeat("把凭据平滑轮换，旧的和新的在截止时间前都能按各自权限工作。", 20)
	if len([]rune(prompt)) <= 240 {
		t.Fatalf("fixture prompt must exceed the drafting limit, got %d runes", len([]rune(prompt)))
	}
	state.Precheck.PromptSHA256 = TextSHA256(prompt)
	prepared, err := PrepareWorkflowPlan(state, template, schema, prompt)
	if err != nil {
		t.Fatalf("a bound platform prompt must be accepted regardless of local draft length: %v", err)
	}
	if prepared.ExecutionPrompt != prompt {
		t.Fatal("frozen platform prompt was altered")
	}
}

func TestPrepareWorkflowPlanRejectsConflictingBindings(t *testing.T) {
	for key, value := range map[string]any{"precheck_item_id": 2410, "repo_url": "https://github.com/other/repo", "commit_ref": "other-baseline", "primary_language": "Rust", "seed_model": "Other Model", "effective_rounds": 100} {
		t.Run(key, func(t *testing.T) {
			state, template, schema, prompt := workflowPlanFixture()
			template.Payload[key] = value
			if _, err := PrepareWorkflowPlan(state, template, schema, prompt); err == nil {
				t.Fatal("contradictory workflow binding accepted")
			}
		})
	}
}

func TestPrepareWorkflowPlanRejectsUnsafeStageSchemaAndFields(t *testing.T) {
	tests := []struct {
		name   string
		change func(*WorkflowState, *SubmissionPlan, *FormSchema, *string)
	}{
		{"frozen stage", func(s *WorkflowState, _ *SubmissionPlan, _ *FormSchema, _ *string) { s.Status = WorkflowDryRunPassed }},
		{"existing state submission", func(s *WorkflowState, _ *SubmissionPlan, _ *FormSchema, _ *string) { s.Submission.ID = 1481 }},
		{"existing draft submission", func(_ *WorkflowState, p *SubmissionPlan, _ *FormSchema, _ *string) { p.ExistingSubmissionID = 1481 }},
		{"schema frozen", func(_ *WorkflowState, _ *SubmissionPlan, schema *FormSchema, _ *string) { schema.Frozen = true }},
		{"schema missing", func(_ *WorkflowState, _ *SubmissionPlan, schema *FormSchema, _ *string) { schema.Version = "" }},
		{"prompt changed", func(_ *WorkflowState, _ *SubmissionPlan, _ *FormSchema, prompt *string) { *prompt += "\n" }},
		{"execution conflict", func(_ *WorkflowState, p *SubmissionPlan, _ *FormSchema, _ *string) {
			p.ExecutionPrompt = "another prompt"
		}},
		{"unknown payload", func(_ *WorkflowState, p *SubmissionPlan, _ *FormSchema, _ *string) {
			p.Payload["local_only"] = "internal"
		}},
		{"auto payload", func(_ *WorkflowState, p *SubmissionPlan, _ *FormSchema, _ *string) {
			p.Payload["requirement_prompt"] = "auto"
		}},
		{"remarks", func(_ *WorkflowState, p *SubmissionPlan, _ *FormSchema, _ *string) {
			p.Payload["备注"] = "local only"
		}},
		{"nested remarks", func(_ *WorkflowState, p *SubmissionPlan, _ *FormSchema, _ *string) {
			p.Payload["title"] = map[string]any{"notes": "local only"}
		}},
		{"attachment remarks", func(_ *WorkflowState, p *SubmissionPlan, _ *FormSchema, _ *string) {
			p.Attachments["remarks"] = "local"
		}},
		{"missing precheck", func(s *WorkflowState, _ *SubmissionPlan, _ *FormSchema, _ *string) { s.Precheck.ItemID = 0 }},
		{"missing baseline", func(s *WorkflowState, _ *SubmissionPlan, _ *FormSchema, _ *string) { s.Project.BaselineCommit = "" }},
		{"schema binding removed", func(_ *WorkflowState, _ *SubmissionPlan, schema *FormSchema, _ *string) {
			schema.Fields = schema.Fields[2:]
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, plan, schema, prompt := workflowPlanFixture()
			test.change(&state, &plan, &schema, &prompt)
			if _, err := PrepareWorkflowPlan(state, plan, schema, prompt); err == nil {
				t.Fatal("unsafe draft preparation accepted")
			}
		})
	}
}

package precheckclient

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reviewRubric() string {
	var lines = []string{"rubrics:"}
	for i := 1; i <= 5; i++ {
		kind := "f2p"
		if i == 5 {
			kind = "p2p"
		}
		lines = append(lines, fmt.Sprintf("- id: %d\n  type: %s\n  text: 输入记录 %d 后得到确定结果，失败时不改变已有内容。", i, kind, i))
	}
	return strings.Join(lines, "\n")
}
func fieldReviewFixture() (SubmissionPlan, FormSchema) {
	schema := FormSchema{Version: "v1", Fields: []FormField{{Key: "repo_url", Type: "url", Required: true}, {Key: "commit_ref", Type: "text", Required: true}, {Key: "commit_url", Type: "text", Required: true}, {Key: "verify_rubric", Type: "textarea", Required: true}, {Key: "artifact_result", Type: "textarea", Required: true}, {Key: "question_type", Type: "select", Options: []string{"有效轮数 > 100", "有效轮数 < 100 且 效果好", "有效轮数 < 100 且 效果差"}, Required: true}, {Key: "effective_rounds", Type: "number", Required: true}, {Key: "requirement_completed", Type: "select", Options: []string{"完成", "部分完成"}, Required: true}, {Key: "task_type", Type: "select", Required: true, Options: []string{"功能新增", "Bug 修复"}}, {Key: "authenticity_note", Type: "textarea", Required: true}, {Key: "involved_modules", Type: "textarea", Required: true}, {Key: "seed_rounds", Type: "number"}, {Key: "trae_session_id_2", Type: "text"}}}
	plan := SubmissionPlan{ExecutionPrompt: "冻结题面", Payload: map[string]any{"repo_url": "https://github.com/original/repo", "commit_ref": strings.Repeat("a", 40), "commit_url": "https://github.com/fork/repo/commit/" + strings.Repeat("b", 40), "verify_rubric": reviewRubric(), "artifact_result": "1 通过\n2 通过\n3 通过\n4 通过\n5 通过", "question_type": "有效轮数 < 100 且 效果好", "effective_rounds": 45, "requirement_completed": "完成", "task_type": "功能新增", "authenticity_note": "原始记录查询只能反映当前数据，需要保存归档条件和结果，改动牵动存储与查询生命周期。", "involved_modules": "数据查询、归档存储、版本比较"}}
	return plan, schema
}
func TestFieldReviewBlocksInvalidRubricResultsAndCrossFieldClaims(t *testing.T) {
	cases := []struct {
		name, key string
		value     any
	}{
		{"invalid YAML", "verify_rubric", "rubrics:\n- id: 1\n type: f2p"},
		{"less than five", "verify_rubric", "rubrics:\n- id: 1\n  type: f2p\n  text: 可观察结果"},
		{"bad IDs", "verify_rubric", strings.Replace(reviewRubric(), "id: 3", "id: 9", 1)},
		{"bad type", "verify_rubric", strings.Replace(reviewRubric(), "f2p", "pass", 1)},
		{"missing text", "verify_rubric", strings.Replace(reviewRubric(), "text:", "note:", 1)},
		{"title leak", "authenticity_note", "参考 Issue #123"},
		{"path leak", "involved_modules", "/Users/test/private/pkg/api"},
		{"results missing", "artifact_result", "1 通过"},
		{"bad punctuation", "artifact_result", "1. 通过\n2 通过\n3 通过\n4 通过\n5 通过"},
		{"failure has no reason", "artifact_result", "1 未通过\n2 通过\n3 通过\n4 通过\n5 通过"},
		{"false completion", "artifact_result", "1 未通过 输入 A 后实际得到 B\n2 通过\n3 通过\n4 通过\n5 通过"},
		{"round mismatch", "effective_rounds", 101},
		{"exactly 100", "effective_rounds", 100},
		{"PR link", "commit_url", "https://github.com/fork/repo/pull/1"},
		{"baseline commit", "commit_url", "https://github.com/fork/repo/commit/" + strings.Repeat("a", 40)},
		{"original repo", "commit_url", "https://github.com/original/repo/commit/" + strings.Repeat("b", 40)},
		{"mixed commit", "commit_ref", "v1.0 (" + strings.Repeat("a", 40) + ")"},
		{"placeholder field", "task_type", "待补充"},
		{"secondary session", "trae_session_id_2", "cannot fill this"},
		{"seed rounds", "seed_rounds", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, s := fieldReviewFixture()
			p.Payload[tc.key] = tc.value
			r := ReviewSubmissionFields(p, s)
			if r.Passed {
				t.Fatalf("invalid %s passed", tc.key)
			}
		})
	}
	p, s := fieldReviewFixture()
	r := ReviewSubmissionFields(p, s)
	if !r.Passed {
		t.Fatal(FieldReviewFailures(r))
	}
	for _, f := range r.Fields {
		if f.Key == "authenticity_note" && f.Status != "review_required" {
			t.Fatal("structure pass was presented as semantic verification")
		}
	}
}
func TestDesignFreezeBindsFourFieldsAndRejectsChangedEvidence(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "source")
	_ = os.Mkdir(workspace, 0700)
	store := NewWorkflowStore(filepath.Join(root, "task-state.json"))
	state, err := store.Init(context.Background(), "design", workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []WorkflowStatus{WorkflowProjectSelected, WorkflowPrecheckDraft, WorkflowPrecheckSubmitted, WorkflowPrecheckPassed} {
		state, err = store.Update(context.Background(), state.Revision, WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *WorkflowState) error {
			s.Status = next
			s.Project = WorkflowProject{RepoURL: "https://github.com/original/repo", BaselineCommit: strings.Repeat("a", 40), Language: "Go"}
			s.Precheck = WorkflowPrecheck{ItemID: 1, PromptSHA256: TextSHA256("冻结题面")}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	schema := FormSchema{Version: "v1", Fields: []FormField{{Key: "repo_url", Type: "url"}, {Key: "precheck_item_id", Type: "number"}, {Key: "commit_ref", Type: "text"}, {Key: "primary_language", Type: "text"}, {Key: "task_type", Type: "select", Options: []string{"功能新增"}}, {Key: "authenticity_note", Type: "textarea"}, {Key: "involved_modules", Type: "textarea"}, {Key: "verify_rubric", Type: "textarea"}}}
	plan, err := PrepareWorkflowPlan(state, SubmissionPlan{Payload: map[string]any{}}, schema, "冻结题面")
	if err != nil {
		t.Fatal(err)
	}
	plan.SchemaSHA256 = FormSchemaSHA256(schema)
	path := filepath.Join(root, "plan.json")
	_ = SaveLocalJSON(path, plan)
	state, err = store.Update(context.Background(), state.Revision, WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *WorkflowState) error { s.Submission.SchemaSHA256 = plan.SchemaSHA256; return nil })
	if err != nil {
		t.Fatal(err)
	}
	state, err = store.BindPlan(context.Background(), state.Revision, path)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "baseline-research.md")
	_ = os.WriteFile(source, []byte("原始固定基线的查询代码研究及路径记录"), 0600)
	evidence, _ := HashWorkflowArtifact(source)
	material := DesignMaterials{SchemaVersion: 1, Origin: "historical_reconstruction", RepoURL: state.Project.RepoURL, BaselineCommit: state.Project.BaselineCommit, PromptSHA256: state.Precheck.PromptSHA256, Fields: map[string]string{"task_type": "功能新增", "authenticity_note": "基线有实时查询，缺少固定版本与比对。", "involved_modules": "查询与持久化", "verify_rubric": reviewRubric()}, Basis: "按冻结需求与原始源码补建，尚未评价模型产物", SourceEvidence: []WorkflowArtifact{evidence}}
	state, err = store.FreezeDesign(context.Background(), material, schema)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = ReadSubmissionPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyDesignBinding(state, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FreezeDesign(context.Background(), material, schema); err == nil {
		t.Fatal("frozen design overwritten")
	}
	changed := plan.Payload["verify_rubric"]
	plan.Payload["verify_rubric"] = "different"
	if VerifyDesignBinding(state, plan) == nil {
		t.Fatal("changed rubric accepted")
	}
	plan.Payload["verify_rubric"] = changed
	if VerifySemanticFields(state, plan, schema) == nil {
		t.Fatal("design freeze replaced missing semantic review")
	}
	_ = os.WriteFile(source, []byte("changed research"), 0600)
	if VerifyDesignBinding(state, plan) == nil {
		t.Fatal("changed source evidence accepted")
	}
}

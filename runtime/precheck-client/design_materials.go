package precheckclient

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

var DesignFieldKeys = []string{"task_type", "authenticity_note", "involved_modules", "verify_rubric"}

type DesignMaterials struct {
	SchemaVersion  int                `json:"schema_version"`
	Origin         string             `json:"origin"`
	RepoURL        string             `json:"repo_url"`
	BaselineCommit string             `json:"baseline_commit"`
	PromptSHA256   string             `json:"prompt_sha256"`
	Fields         map[string]string  `json:"fields"`
	Basis          string             `json:"basis"`
	SourceEvidence []WorkflowArtifact `json:"source_evidence"`
	FrozenAt       string             `json:"frozen_at,omitempty"`
}

func validateDesign(m DesignMaterials, state WorkflowState, schema FormSchema) error {
	if m.SchemaVersion != 1 || m.RepoURL != state.Project.RepoURL || m.BaselineCommit != state.Project.BaselineCommit || m.PromptSHA256 != state.Precheck.PromptSHA256 {
		return fmt.Errorf("设计材料与冻结题面、原始仓库或基线不一致")
	}
	if m.Origin != "before_execution" && m.Origin != "historical_reconstruction" {
		return fmt.Errorf("设计材料必须标明执行前设计或历史补建")
	}
	if m.Origin == "before_execution" && state.Status != WorkflowPrecheckPassed {
		return fmt.Errorf("已有执行不能冒充执行前设计")
	}
	if isPlaceholder(m.Basis) || len(m.SourceEvidence) == 0 {
		return fmt.Errorf("必须记录原始基线研究依据及文件证据，不能按产物倒推题目")
	}
	fields := map[string]FormField{}
	for _, f := range schema.Fields {
		fields[f.Key] = f
	}
	for _, key := range DesignFieldKeys {
		text := m.Fields[key]
		f, ok := fields[key]
		if !ok || isPlaceholder(text) || sourceLeak.MatchString(text) {
			return fmt.Errorf("设计字段 %s 缺失、含来源泄露或不在 schema 中", key)
		}
		if err := validateOneField(f, text); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	if len(m.Fields) != len(DesignFieldKeys) {
		return fmt.Errorf("设计材料只能包含四个设计字段，不能夹带产物结果")
	}
	if _, err := ParseRubrics(m.Fields["verify_rubric"]); err != nil {
		return err
	}
	return verifyFileEvidence(m.SourceEvidence)
}
func verifyFileEvidence(evidence []WorkflowArtifact) error {
	for _, saved := range evidence {
		current, err := HashWorkflowArtifact(saved.Path)
		if err != nil || !reflect.DeepEqual(current, saved) {
			return fmt.Errorf("研究或字段审核文件证据变化，需要重新核对")
		}
	}
	return nil
}
func VerifyDesignBinding(state WorkflowState, plan SubmissionPlan) error {
	if state.Verification.DesignPath == "" || state.Verification.DesignSHA256 == "" {
		return fmt.Errorf("四项设计材料尚未冻结，不能提交")
	}
	artifact, err := HashWorkflowArtifact(state.Verification.DesignPath)
	if err != nil || artifact.SHA256 != state.Verification.DesignSHA256 {
		return fmt.Errorf("冻结设计材料已变化")
	}
	var m DesignMaterials
	if ReadLocalJSON(state.Verification.DesignPath, &m) != nil || m.PromptSHA256 != state.Precheck.PromptSHA256 || m.BaselineCommit != state.Project.BaselineCommit || m.RepoURL != state.Project.RepoURL || m.FrozenAt == "" {
		return fmt.Errorf("冻结设计与本题不一致")
	}
	for _, key := range DesignFieldKeys {
		if plan.Payload[key] != m.Fields[key] {
			return fmt.Errorf("提交字段 %s 与冻结设计材料不一致", key)
		}
	}
	rubric, err := os.ReadFile(filepath.Join(filepath.Dir(state.Verification.DesignPath), "rubric.yaml"))
	if err != nil || string(rubric) != m.Fields["verify_rubric"] {
		return fmt.Errorf("rubric.yaml 与冻结设计不一致")
	}
	return verifyFileEvidence(m.SourceEvidence)
}
func (s *WorkflowStore) FreezeDesign(ctx context.Context, input DesignMaterials, schema FormSchema) (WorkflowState, error) {
	state, err := s.Load(ctx)
	if err != nil {
		return state, err
	}
	if state.Verification.DesignPath != "" {
		return state, fmt.Errorf("设计材料已经冻结，不能覆盖或针对产物新增标准")
	}
	if state.Submission.ID != 0 || (state.PendingOperation != nil && (state.PendingOperation.Outcome == "started" || state.PendingOperation.Outcome == "uncertain")) || frozenWorkflowMaterials(state.Status) {
		return state, fmt.Errorf("当前阶段不能冻结新设计")
	}
	if err := validateDesign(input, state, schema); err != nil {
		return state, err
	}
	if !FormSchemaMatches(state.Submission.SchemaSHA256, schema) {
		return state, fmt.Errorf("设计 schema 未绑定")
	}
	plan, err := ReadSubmissionPlan(state.PlanPath)
	if err != nil {
		return state, err
	}
	for _, key := range DesignFieldKeys {
		if v := plan.Payload[key]; v != nil && v != "" && v != input.Fields[key] {
			return state, fmt.Errorf("已有 %s 与待冻结设计冲突，不能覆盖", key)
		}
		plan.Payload[key] = input.Fields[key]
	}
	root := filepath.Join(filepath.Dir(s.Path), "design")
	if err := os.Mkdir(root, 0700); err != nil {
		return state, fmt.Errorf("设计目录已存在或不可建立，请核查")
	}
	input.FrozenAt = time.Now().UTC().Format(time.RFC3339Nano)
	path := filepath.Join(root, "materials.json")
	if err := SaveLocalJSON(path, input); err != nil {
		return state, err
	}
	if err := os.WriteFile(filepath.Join(root, "rubric.yaml"), []byte(input.Fields["verify_rubric"]), 0600); err != nil {
		return state, err
	}
	artifact, err := HashWorkflowArtifact(path)
	if err != nil {
		return state, err
	}
	if err := SaveLocalJSON(state.PlanPath, plan); err != nil {
		return state, err
	}
	state, err = s.BindPlan(ctx, state.Revision, state.PlanPath)
	if err != nil {
		return state, err
	}
	return s.Update(ctx, state.Revision, WorkflowAudit{Action: "design_frozen", Outcome: "success", PayloadSHA256: artifact.SHA256}, func(next *WorkflowState) error {
		next.Verification.DesignPath = path
		next.Verification.DesignSHA256 = artifact.SHA256
		return nil
	})
}

type SemanticFieldReview struct {
	SchemaVersion int                            `json:"schema_version"`
	PlanSHA256    string                         `json:"plan_sha256"`
	PromptSHA256  string                         `json:"prompt_sha256"`
	Evidence      map[string]FieldReviewEvidence `json:"evidence"`
}
type FieldReviewEvidence struct {
	Conclusion string             `json:"conclusion"`
	Sources    []WorkflowArtifact `json:"sources"`
}

func VerifySemanticFields(state WorkflowState, plan SubmissionPlan, schema FormSchema) error {
	var review SemanticFieldReview
	path := filepath.Join(filepath.Dir(state.PlanPath), "field-review-evidence.json")
	if ReadLocalJSON(path, &review) != nil || review.SchemaVersion != 1 || review.PlanSHA256 != PlanSHA256(plan) || review.PromptSHA256 != state.Precheck.PromptSHA256 {
		return fmt.Errorf("缺少与当前计划匹配的逐字段语义复核记录")
	}
	for _, f := range ReviewSubmissionFields(plan, schema).Fields {
		if f.Status != "review_required" {
			continue
		}
		evidence := review.Evidence[f.Key]
		if strings.TrimSpace(evidence.Conclusion) == "" || len(evidence.Sources) == 0 {
			return fmt.Errorf("字段 %s 缺少实际代码、日志或验收证据", f.Key)
		}
		if err := verifyFileEvidence(evidence.Sources); err != nil {
			return fmt.Errorf("字段 %s: %w", f.Key, err)
		}
	}
	return nil
}

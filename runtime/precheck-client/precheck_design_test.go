package precheckclient

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrecheckDesignStoresEvidenceBindsPassedPromptAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := &PrecheckDesignStore{Root: filepath.Join(root, "registry")}
	source := filepath.Join(root, "research.md")
	_ = os.WriteFile(source, []byte("固定基线的实际研究记录"), 0600)
	evidence, _ := HashWorkflowArtifact(source)
	_, schema := fieldReviewFixture()
	prompt := "归档查询时保存固定版本，并可比较当前数据的变化。"
	material := DesignMaterials{SchemaVersion: 1, Origin: "before_execution", RepoURL: "https://github.com/original/repo", BaselineCommit: strings.Repeat("a", 40), PromptSHA256: TextSHA256(prompt), Fields: map[string]string{"task_type": "功能新增", "authenticity_note": "基线实时计算查询，归档需要跨查询和持久化。", "involved_modules": "数据查询与快照持久化", "verify_rubric": reviewRubric()}, Basis: "出题时读取固定源码并设计验收", SourceEvidence: []WorkflowArtifact{evidence}}
	record, err := store.Save(ctx, prompt, "Go", material, schema)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.RecordPath(record))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("record not private")
	}
	if record.Materials.SourceEvidence[0].Path == source {
		t.Fatal("evidence retained a mutable original file path")
	}
	_ = os.Remove(source)
	record, err = (&PrecheckDesignStore{Root: store.Root}).Find(ctx, material.RepoURL, prompt)
	if err != nil {
		t.Fatal("snapshot broken after original removed", err)
	}
	item := ItemDetail{BatchItem: BatchItem{ID: 55, RepoURL: material.RepoURL, Status: "PRECHECK_SUBMITTED"}, RequirementPrompt: prompt}
	if _, err := store.Bind(ctx, item); err == nil {
		t.Fatal("unpassed precheck was bound")
	}
	item.Status = "PRECHECK_PASSED"
	if bound, err := store.Bind(ctx, item); err != nil || !bound {
		t.Fatalf("bind %v %v", bound, err)
	}
	if bound, err := store.Bind(ctx, item); err != nil || !bound {
		t.Fatal("idempotent bind failed")
	}
	restored, err := store.Approved(ctx, 55, material.RepoURL, TextSHA256(prompt))
	if err != nil || restored.Materials.Fields["verify_rubric"] != reviewRubric() {
		t.Fatalf("restored %v %v", restored, err)
	}
	if _, err := store.Approved(ctx, 55, material.RepoURL, TextSHA256("another prompt")); err == nil {
		t.Fatal("wrong prompt reused design")
	}
	if _, err := store.Approved(ctx, 55, "https://github.com/other/repo", TextSHA256(prompt)); err == nil {
		t.Fatal("wrong repo reused design")
	}
	material.SourceEvidence = record.Materials.SourceEvidence
	again, err := store.Save(ctx, prompt, "Go", material, schema)
	if err != nil || again.RecordID != record.RecordID {
		t.Fatal("same design could not resume", err)
	}
	material.Fields["authenticity_note"] = "different design"
	if _, err := store.Save(ctx, prompt, "Go", material, schema); err == nil {
		t.Fatal("existing design overwritten")
	}
	_ = os.WriteFile(restored.Materials.SourceEvidence[0].Path, []byte("tampered"), 0600)
	if _, err := store.Approved(ctx, 55, material.RepoURL, TextSHA256(prompt)); err == nil {
		t.Fatal("changed cached evidence accepted")
	}
}

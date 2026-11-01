package precheckclient

import (
	"context"
	"path/filepath"
	"testing"
)

func recoveryFixture(t *testing.T, kind string) (WorkflowState, ItemDetail) {
	t.Helper()
	repo, prompt := "https://github.com/example/repo", "actual recovery prompt"
	hash, err := RowsSHA256([]Row{{RepoURL: repo, Prompt: prompt}})
	if err != nil {
		t.Fatal(err)
	}
	state := WorkflowState{
		Status:           WorkflowPrecheckDraft,
		Project:          WorkflowProject{RepoURL: repo},
		PendingOperation: &WorkflowOperation{ID: "operation", Kind: kind, PayloadSHA256: hash, StartedAt: "2026-10-06T08:00:00Z", Outcome: "uncertain", PrecheckSnapshotTaken: true, PriorPrecheckItemIDs: []int{1, 2}},
	}
	if kind != "precheck_submit" {
		state.PendingOperation.PrecheckBefore = &WorkflowPrecheck{ItemID: 2, BatchID: "old-batch", PromptSHA256: TextSHA256(prompt)}
	}
	item := ItemDetail{BatchItem: BatchItem{ID: 3, BatchID: "new-batch", RepoURL: repo, CreatedAt: "2026-10-06T08:00:01Z", UpdatedAt: "2026-10-06T08:00:01Z"}, RequirementPrompt: prompt}
	return state, item
}

func TestPrecheckRecoveryRequiresEvidenceOfNewRun(t *testing.T) {
	tests := []struct {
		name   string
		kind   string
		change func(*WorkflowState, *ItemDetail)
		want   bool
	}{
		{"new submission", "precheck_submit", nil, true},
		{"existing matching submission", "precheck_submit", func(s *WorkflowState, i *ItemDetail) { i.ID = 2 }, false},
		{"new batch for rerun", "precheck_rerun", func(s *WorkflowState, i *ItemDetail) { i.ID = 2 }, true},
		{"same run with newer timestamp", "precheck_rerun", func(s *WorkflowState, i *ItemDetail) { i.ID = 2; i.BatchID = "old-batch" }, false},
		{"same run after repair", "precheck_repair", func(s *WorkflowState, i *ItemDetail) { i.ID = 2; i.BatchID = "old-batch" }, false},
		{"new repair record", "precheck_repair", nil, true},
		{"content mismatch", "precheck_repair", func(s *WorkflowState, i *ItemDetail) { i.RequirementPrompt += " different" }, false},
		{"repository mismatch", "precheck_repair", func(s *WorkflowState, i *ItemDetail) { i.RepoURL += "-different" }, false},
		{"record predates request", "precheck_submit", func(s *WorkflowState, i *ItemDetail) { i.CreatedAt = "2026-10-06T07:59:59Z" }, false},
		{"same item lacks updated time", "precheck_repair", func(s *WorkflowState, i *ItemDetail) { i.ID = 2; i.UpdatedAt = "" }, false},
		{"legacy snapshot missing", "precheck_rerun", func(s *WorkflowState, i *ItemDetail) { s.PendingOperation.PrecheckSnapshotTaken = false }, false},
		{"completed write", "precheck_submit", func(s *WorkflowState, i *ItemDetail) { s.PendingOperation.Outcome = "success" }, false},
		{"not a precheck operation", "submit", nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, item := recoveryFixture(t, test.kind)
			if test.change != nil {
				test.change(&state, &item)
			}
			if err := ValidatePrecheckRecovery(state, item); (err == nil) != test.want {
				t.Fatalf("recovery valid=%v; want=%v", err == nil, test.want)
			}
			if state.PendingOperation.Outcome != "uncertain" && test.name != "completed write" {
				t.Fatal("validation mutated operation outcome")
			}
		})
	}
}

func TestPrecheckSnapshotCannotChangeWhileUnresolved(t *testing.T) {
	ctx := context.Background()
	store := NewWorkflowStore(filepath.Join(t.TempDir(), "task-state.json"))
	state, err := store.Init(ctx, "recovery-task", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, err = BeginPrecheckWrite(ctx, store, state, "precheck_submit", "/precheck/batches", TextSHA256("request"), nil, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	state, err = FinishWorkflowWrite(ctx, store, state, "/precheck/batches", 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(ctx, state.Revision, WorkflowAudit{Action: "edit", Outcome: "success"}, func(s *WorkflowState) error {
		s.PendingOperation.PriorPrecheckItemIDs = []int{1}
		return nil
	})
	if err == nil {
		t.Fatal("unresolved snapshot was mutable")
	}
	loaded, err := store.Load(ctx)
	if err != nil || len(loaded.PendingOperation.PriorPrecheckItemIDs) != 2 || loaded.Revision != state.Revision {
		t.Fatal("rejected mutation changed persistent state")
	}
}

package precheckclient

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newWorkflowTestStore(t *testing.T) (*WorkflowStore, string) {
	t.Helper()
	dir := t.TempDir()
	return NewWorkflowStore(filepath.Join(dir, "task-state.json")), dir
}

func TestWorkflowInitAndAdvance(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "task-a", dir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 1 || state.Status != WorkflowInitialized {
		t.Fatalf("unexpected initial state: %+v", state)
	}
	if info, err := os.Stat(filepath.Join(dir, "task-state.json")); err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("state permissions are not private: %v", err)
	}
	if _, err := store.Advance(ctx, state.Revision, WorkflowPrecheckPassed, WorkflowAudit{Action: "advance", Outcome: "success"}); err == nil {
		t.Fatal("invalid transition was accepted")
	}
	state, err = store.Advance(ctx, state.Revision, WorkflowProjectSelected, WorkflowAudit{Action: "advance", Outcome: "success"})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != state.Revision || loaded.LastEventSHA256 == "" {
		t.Fatalf("journal state was not recovered: %+v", loaded)
	}
	if lines, err := os.ReadFile(filepath.Join(dir, "events.jsonl")); err != nil || len(lines) == 0 || !strings.Contains(string(lines), `"external_write":false`) {
		t.Fatalf("journal is missing structured event: %v", err)
	}
}

func TestWorkflowRevisionAndJournalTamper(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "task-b", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(ctx, state.Revision-1, WorkflowProjectSelected, WorkflowAudit{Action: "advance"}); err == nil {
		t.Fatal("stale revision was accepted")
	}
	if _, err := store.Advance(ctx, state.Revision, WorkflowProjectSelected, WorkflowAudit{Action: "advance"}); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(dir, "events.jsonl")
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"to":"project_selected"`, `"to":"closed"`, 1))
	if err := os.WriteFile(journalPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx); err == nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("tampered journal was accepted: %v", err)
	}
}

func TestWorkflowAuditRejectsSecretsAndUncertainRetry(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "task-c", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, state.Revision, WorkflowAudit{Action: "write", ExternalWrite: true, Method: "POST", Endpoint: "/submissions/create", OperationID: "op-1", PayloadSHA256: strings.Repeat("a", 64)}, func(next *WorkflowState) error {
		next.PendingOperation = &WorkflowOperation{ID: "op-1", Kind: "submit", PayloadSHA256: strings.Repeat("a", 64), StartedAt: "2026-01-01T00:00:00Z", Outcome: "uncertain"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state, err = store.Load(ctx)
	if err != nil || state.PendingOperation == nil {
		t.Fatalf("pending operation was not saved: %v", err)
	}
	if _, err := store.Update(ctx, state.Revision, WorkflowAudit{Action: "write", ExternalWrite: true, Method: "POST", Endpoint: "/submissions/create", OperationID: "op-2", PayloadSHA256: strings.Repeat("b", 64)}, func(next *WorkflowState) error {
		next.PendingOperation = &WorkflowOperation{ID: "op-2", Kind: "submit", PayloadSHA256: strings.Repeat("b", 64), StartedAt: "2026-01-01T00:00:00Z", Outcome: "started"}
		return nil
	}); err == nil {
		t.Fatal("uncertain operation was replaced")
	}
	if _, err := store.Update(ctx, state.Revision, WorkflowAudit{Action: "write", ExternalWrite: true, Method: "POST", Endpoint: "https://secret.invalid/create", OperationID: "op-3", PayloadSHA256: strings.Repeat("c", 64)}, func(next *WorkflowState) error { return nil }); err == nil {
		t.Fatal("secret endpoint was accepted")
	}
}

func TestWorkflowBindPlanHashesOnly(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "task-d", dir)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "submission-plan.json")
	plan := SubmissionPlan{SchemaVersion: "1", ExecutionPrompt: "真实执行题面", Payload: map[string]any{"repo_url": "https://github.com/example/repo", "precheck_item_id": 12.0, "effective_rounds": 4.0}, Attachments: map[string]string{}}
	data, _ := json.Marshal(plan)
	if err := os.WriteFile(planPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	state, err = store.BindPlan(ctx, state.Revision, planPath)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if state.PlanSHA256 == "" || state.Precheck.PromptSHA256 == "" || state.PlanPath != resolved {
		t.Fatalf("plan was not bound: %+v", state)
	}
	serialized, _ := json.Marshal(state)
	if strings.Contains(string(serialized), "真实执行题面") {
		t.Fatal("workflow state copied prompt content instead of its hash")
	}
}

func TestWorkflowBindPlanRequiresMatchingSchemaSnapshot(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "schema-binding", dir)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "submission-plan.json")
	writePlan := func(schemaHash string) {
		t.Helper()
		plan := SubmissionPlan{
			SchemaVersion:   "1",
			SchemaSHA256:    schemaHash,
			ExecutionPrompt: "真实执行题面",
			Payload:         map[string]any{"repo_url": "https://github.com/example/repo", "precheck_item_id": 12.0},
			Attachments:     map[string]string{},
		}
		data, _ := json.Marshal(plan)
		if err := os.WriteFile(planPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}

	writePlan(strings.Repeat("a", 64))
	if _, err := store.BindPlan(ctx, state.Revision, planPath); err == nil {
		t.Fatal("plan schema was accepted without a state schema snapshot")
	}
	state, err = store.Update(ctx, state.Revision, WorkflowAudit{Action: "schema_fetch", Outcome: "success"}, func(next *WorkflowState) error {
		next.Submission.SchemaSHA256 = strings.Repeat("b", 64)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	writePlan("")
	if _, err := store.BindPlan(ctx, state.Revision, planPath); err == nil {
		t.Fatal("plan without schema was accepted with a state schema snapshot")
	}
	writePlan(strings.Repeat("b", 64))
	if _, err := store.BindPlan(ctx, state.Revision, planPath); err != nil {
		t.Fatal(err)
	}
}

func TestFrozenMaterialsIgnoreSubmissionOutcomeButBindSchema(t *testing.T) {
	hash := strings.Repeat("a", 64)
	base := WorkflowState{
		PlanPath:   "/tmp/plan.json",
		PlanSHA256: hash,
		Submission: WorkflowSubmission{SchemaPath: "/tmp/schema.json", SchemaSHA256: hash, AuditPath: "/tmp/audit.json", AuditSHA256: hash, AuditedPlanSHA256: hash},
	}
	finished := base
	finished.Submission.ID = 42
	finished.Submission.Status = "PENDING_QC"
	finished.Submission.ReceiptPath = "/tmp/receipt.json"
	if !sameWorkflowMaterials(base, finished) {
		t.Fatal("submission outcome fields changed the frozen material identity")
	}
	changedSchema := base
	changedSchema.Submission.SchemaSHA256 = strings.Repeat("b", 64)
	if sameWorkflowMaterials(base, changedSchema) {
		t.Fatal("schema snapshot drift was not detected")
	}
}

func TestWorkflowInterruptedAppendAndSnapshotRecovery(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "recover", dir)
	if err != nil {
		t.Fatal(err)
	}
	store.journalWriter = func(file *os.File, data []byte) error {
		if _, err := file.Write(data[:len(data)/2]); err != nil {
			return err
		}
		return errors.New("injected interrupted append")
	}
	if _, err := store.Advance(ctx, state.Revision, WorkflowProjectSelected, WorkflowAudit{Action: "advance"}); err == nil {
		t.Fatal("journal write failure was ignored")
	}
	store.journalWriter = nil
	loaded, err := store.Load(ctx)
	if err != nil || loaded.Revision != state.Revision {
		t.Fatalf("partial append was not discarded: %v", err)
	}
	store.snapshotWriter = func(string, any) error { return errors.New("injected snapshot failure") }
	if _, err := store.Advance(ctx, loaded.Revision, WorkflowProjectSelected, WorkflowAudit{Action: "advance"}); err == nil {
		t.Fatal("snapshot write failure was ignored")
	}
	store.snapshotWriter = nil
	loaded, err = store.Load(ctx)
	if err != nil || loaded.Status != WorkflowProjectSelected || loaded.Revision != state.Revision+1 {
		t.Fatalf("committed journal did not recover snapshot: %v", err)
	}
}

func TestWorkflowConcurrentRevisionOnlyOneWins(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "concurrent", dir)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := NewWorkflowStore(store.Path).Advance(ctx, state.Revision, WorkflowProjectSelected, WorkflowAudit{Action: "advance"})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("expected one winning revision, got %d", wins)
	}
}

func TestWorkflowUncertainCannotBeClearedOrChanged(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "uncertain", dir)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	state, err = store.Update(ctx, state.Revision, WorkflowAudit{Action: "write_started", Outcome: "started"}, func(next *WorkflowState) error {
		next.PendingOperation = &WorkflowOperation{ID: "operation-a", Kind: "submit", PayloadSHA256: hash, StartedAt: time.Now().UTC().Format(time.RFC3339), Outcome: "uncertain"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, state.Revision, WorkflowAudit{Action: "write_finished", OperationID: "operation-a", Outcome: "success"}, func(next *WorkflowState) error {
		next.PendingOperation = nil
		return nil
	}); err == nil {
		t.Fatal("uncertain operation cleared without reconciliation")
	}
	if _, err := store.Update(ctx, state.Revision, WorkflowAudit{Action: "write_finished", OperationID: "operation-a", Outcome: "success"}, func(next *WorkflowState) error {
		next.PendingOperation.Outcome = "success"
		return nil
	}); err == nil {
		t.Fatal("uncertain operation resolved without reconciliation")
	}
	if _, err := store.Update(ctx, state.Revision, WorkflowAudit{Action: "reconcile", OperationID: "operation-a", Outcome: "reconciled"}, func(next *WorkflowState) error {
		next.PendingOperation = nil
		return nil
	}); err != nil {
		t.Fatalf("explicit reconciliation rejected: %v", err)
	}
}

func TestWorkflowAuditedMaterialsFreeze(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "frozen", dir)
	if err != nil {
		t.Fatal(err)
	}
	steps := []WorkflowStatus{WorkflowProjectSelected, WorkflowPrecheckDraft, WorkflowPrecheckSubmitted, WorkflowPrecheckPassed, WorkflowTraeRunning, WorkflowSessionCaptured, WorkflowSessionVerified, WorkflowArtifactsVerified, WorkflowDryRunPassed}
	for _, status := range steps {
		state, err = store.Advance(ctx, state.Revision, status, WorkflowAudit{Action: "advance"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Update(ctx, state.Revision, WorkflowAudit{Action: "bind_plan"}, func(next *WorkflowState) error {
		next.PlanSHA256 = strings.Repeat("a", 64)
		return nil
	}); err == nil {
		t.Fatal("audited plan replaced without invalidation")
	}
	if _, err := store.Advance(ctx, state.Revision, WorkflowArtifactsVerified, WorkflowAudit{Action: "invalidate_audit"}); err != nil {
		t.Fatalf("material audit invalidation rejected: %v", err)
	}
}

func TestWorkflowHistoricalImportIsSingleLocalReceiptEvent(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	for _, name := range []string{"artifact.png", "trace.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("synthetic artifact"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	planPath, receiptPath := filepath.Join(dir, "plan.json"), filepath.Join(dir, "receipt.json")
	plan := SubmissionPlan{SchemaVersion: "1", ExistingSubmissionID: 34, ExecutionPrompt: "实际历史题面", Payload: map[string]any{"repo_url": "https://github.com/example/repo", "precheck_item_id": 12, "effective_rounds": 45, "trae_session_id": syntheticToken()}, Attachments: map[string]string{"x_artifact_shot": "artifact.png", "x_trae_trace": "trace.md"}}
	data, _ := json.Marshal(plan)
	if err := os.WriteFile(planPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	receipt := map[string]any{
		"updated_at":          "2026-10-05T20:15:00+08:00",
		"server_verification": map[string]any{"new_submission_id": 34, "status": "PENDING_FIX", "session_sha256": bytesSHA256([]byte(syntheticToken())), "effective_rounds": 45, "precheck_item_id": 12, "session_exact_match": true, "prompt_exact_match": true, "artifact_shot_saved": true, "trace_saved": true, "remarks_in_server_values": false},
	}
	data, _ = json.Marshal(receipt)
	if err := os.WriteFile(receiptPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	state, err := store.ImportHistorical(ctx, "historic", dir, planPath, receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != WorkflowQCRejected || state.Revision != 1 || state.Submission.ID != 34 || state.Submission.Source != "local_saved_receipt" || state.Submission.ObservedAt != receipt["updated_at"] {
		t.Fatalf("unexpected imported state: %+v", state)
	}
	journal, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil || strings.Count(string(journal), "\n") != 1 || strings.Contains(string(journal), syntheticToken()) || !strings.Contains(string(journal), `"action":"historical_import"`) {
		t.Fatalf("historical journal is incorrect: %v", err)
	}
	loaded, err := store.Load(ctx)
	if err != nil || loaded.Submission.ReceiptSHA256 == "" {
		t.Fatalf("historical state could not reload: %v", err)
	}
	if _, err := store.ImportHistorical(ctx, "historic", dir, planPath, receiptPath); err == nil {
		t.Fatal("duplicate historical import was accepted")
	}
	for _, mismatch := range []struct {
		field string
		value any
	}{
		{"new_submission_id", 99},
		{"precheck_item_id", 99},
		{"effective_rounds", 100},
		{"session_sha256", strings.Repeat("a", 64)},
		{"remarks_in_server_values", true},
		{"session_exact_match", false},
	} {
		t.Run(mismatch.field, func(t *testing.T) {
			badStore, badDir := newWorkflowTestStore(t)
			var badReceipt map[string]any
			original, _ := json.Marshal(receipt)
			_ = json.Unmarshal(original, &badReceipt)
			badReceipt["server_verification"].(map[string]any)[mismatch.field] = mismatch.value
			badBytes, _ := json.Marshal(badReceipt)
			badReceiptPath := filepath.Join(badDir, "receipt.json")
			if err := os.WriteFile(badReceiptPath, badBytes, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := badStore.ImportHistorical(ctx, "bad-history", dir, planPath, badReceiptPath); err == nil {
				t.Fatal("mismatched receipt was accepted")
			}
			if _, err := os.Stat(filepath.Join(badDir, "events.jsonl")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed import committed an event")
			}
		})
	}
}

func TestWorkflowLocalOnlyStateIsTerminalButReversible(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "task-local-only", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []WorkflowStatus{WorkflowProjectSelected, WorkflowPrecheckDraft, WorkflowPrecheckSubmitted, WorkflowPrecheckPassed, WorkflowTraeRunning, WorkflowSessionCaptured, WorkflowSessionVerified} {
		state, err = store.Advance(ctx, state.Revision, status, WorkflowAudit{Action: "test_setup", Outcome: "success"})
		if err != nil {
			t.Fatalf("advance to %s: %v", status, err)
		}
	}
	if !LocalOnlyEligible(state.Status) {
		t.Fatalf("%s is not eligible for the local-only decision", state.Status)
	}
	state, err = store.Advance(ctx, state.Revision, WorkflowLocalOnly, WorkflowAudit{Action: "local_only", Outcome: "success"})
	if err != nil || state.Status != WorkflowLocalOnly {
		t.Fatalf("local only: %v %+v", err, state)
	}
	// Reopening returns the task to the draft stage so its round count can be
	// corrected; the journal keeps both transitions.
	reopened, err := store.Advance(ctx, state.Revision, WorkflowSessionCaptured, WorkflowAudit{Action: "reopen_local_only", Outcome: "success"})
	if err != nil || reopened.Status != WorkflowSessionCaptured {
		t.Fatalf("reopen: %v %+v", err, reopened)
	}
	loaded, err := store.Load(ctx)
	if err != nil || loaded.Status != WorkflowSessionCaptured {
		t.Fatalf("reloaded: %v %+v", err, loaded)
	}
}

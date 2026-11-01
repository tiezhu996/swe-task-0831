package precheckclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readyWorkflow(t *testing.T) (*WorkflowStore, SubmissionPlan, FormSchema, AuditEvidence) {
	return readyWorkflowWithRounds(t, 0)
}

// readyWorkflowWithRounds builds the same audited task with a chosen verified
// round count, so tests can exercise the collection policy without rewriting a
// frozen dry-run state.
func readyWorkflowWithRounds(t *testing.T, rounds int) (*WorkflowStore, SubmissionPlan, FormSchema, AuditEvidence) {
	t.Helper()
	p, s, e := auditedFixture(t)
	if rounds > 0 {
		p.Payload["effective_rounds"] = rounds
		e.Session.EffectiveRoundCount = rounds
	}
	dir := t.TempDir()
	planPath := filepath.Join(dir, "submission-plan.json")
	if err := SaveLocalJSON(planPath, p); err != nil {
		t.Fatal(err)
	}
	store := NewWorkflowStore(filepath.Join(dir, "task-state.json"))
	ctx := context.Background()
	state, err := store.Init(ctx, "synthetic-task", e.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []WorkflowStatus{WorkflowProjectSelected, WorkflowPrecheckDraft, WorkflowPrecheckSubmitted, WorkflowPrecheckPassed, WorkflowTraeRunning, WorkflowSessionCaptured, WorkflowSessionVerified, WorkflowArtifactsVerified} {
		state, err = store.Advance(ctx, state.Revision, status, WorkflowAudit{Action: "test_setup", Outcome: "success"})
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err = store.BindPlan(ctx, state.Revision, planPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(ctx, state.Revision, WorkflowAudit{Action: "test_audit", Outcome: "success"}, func(s *WorkflowState) error {
		if rounds > 0 {
			s.Trae.EffectiveRounds = &rounds
		}
		s.Status = WorkflowDryRunPassed
		s.Submission.AuditedPlanSHA256 = PlanSHA256(p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, p, s, e
}

func TestWorkflowSubmissionPersistsIDAndRejectsRepeat(t *testing.T) {
	store, p, s, e := readyWorkflow(t)
	var uploads, creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/precheck/items/2409":
			fmt.Fprint(w, `{"code":0,"data":{"status":"PRECHECK_PASSED","repo_url":"https://github.com/example/repo","requirement_prompt":"actual prompt"}}`)
		case "/uploads/attachment":
			uploads.Add(1)
			fmt.Fprint(w, `{"code":0,"data":{"url":"https://example.com/attachment"}}`)
		case "/submissions/create":
			creates.Add(1)
			_ = r.ParseMultipartForm(1 << 20)
			var payload map[string]any
			_ = json.Unmarshal([]byte(r.FormValue("payload")), &payload)
			if payload["trae_session_id"] != syntheticToken() {
				t.Error("token not preserved")
			}
			if _, ok := payload["remarks"]; ok {
				t.Error("remarks sent")
			}
			fmt.Fprint(w, `{"code":0,"data":{"id":345,"status":"PENDING_QC"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, _ := New(server.URL, nil)
	c.SetSessionCookies("synthetic", "csrf")
	result, err := c.SubmitWorkflow(context.Background(), store, p, s, e)
	if err != nil || result.ID != 345 {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background())
	if err != nil || state.Submission.ID != 345 || state.Status != WorkflowSubmitted {
		t.Fatal(err)
	}
	var saved SubmissionPlan
	if err := ReadLocalJSON(state.PlanPath, &saved); err != nil || saved.ExistingSubmissionID != 345 {
		t.Fatal(err)
	}
	_, err = c.SubmitWorkflow(context.Background(), store, p, s, e)
	if err == nil {
		t.Fatal("repeat accepted")
	}
	if creates.Load() != 1 || uploads.Load() != 2 {
		t.Fatalf("writes uploads=%d creates=%d", uploads.Load(), creates.Load())
	}
	journal, _ := os.ReadFile(store.eventsPath())
	if strings.Contains(string(journal), syntheticToken()) {
		t.Fatal("token leaked")
	}
}

func TestWorkflowLimitedSubmissionStopsBeforeAnyWrite(t *testing.T) {
	store, p, s, e := readyWorkflow(t)
	s.Limited = true
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected network request", http.StatusInternalServerError)
	}))
	defer server.Close()
	c, _ := New(server.URL, nil)
	c.SetSessionCookies("synthetic", "csrf")
	if _, err := c.SubmitWorkflow(context.Background(), store, p, s, e); err == nil || !strings.Contains(err.Error(), "platform currently blocks submission") {
		t.Fatalf("limited submission was not blocked locally: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("limited submission reached the network")
	}
	state, err := store.Load(context.Background())
	if err != nil || state.PendingOperation != nil || state.Status != WorkflowDryRunPassed {
		t.Fatalf("limited submission changed workflow state: %v", err)
	}
}

func TestWorkflowUnknownWriteCannotBeRetried(t *testing.T) {
	store, p, s, e := readyWorkflow(t)
	var uploads, creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"code":0,"data":{"status":"PRECHECK_PASSED","repo_url":"https://github.com/example/repo","requirement_prompt":"actual prompt"}}`)
			return
		}
		if r.URL.Path == "/uploads/attachment" {
			uploads.Add(1)
			http.Error(w, "uncertain", 500)
			return
		}
		creates.Add(1)
	}))
	defer server.Close()
	c, _ := New(server.URL, nil)
	c.SetSessionCookies("synthetic", "csrf")
	_, err := c.SubmitWorkflow(context.Background(), store, p, s, e)
	if err == nil {
		t.Fatal("failure accepted")
	}
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.PendingOperation == nil || state.PendingOperation.Outcome != "uncertain" {
		t.Fatal("unknown write not saved")
	}
	if state.PendingOperation.CreateRequestStarted == nil || *state.PendingOperation.CreateRequestStarted {
		t.Fatal("failed upload did not retain explicit proof that create was never started")
	}
	_, err = c.SubmitWorkflow(context.Background(), store, p, s, e)
	if err == nil {
		t.Fatal("repeat accepted")
	}
	if uploads.Load() != 1 || creates.Load() != 0 {
		t.Fatal("unknown write retried")
	}
}

func TestWorkflowFailedAttachmentCanResetBeforeCreateAndResubmit(t *testing.T) {
	store, p, s, e := readyWorkflow(t)
	var requests, uploads, creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/precheck/items/2409":
			fmt.Fprint(w, `{"code":0,"data":{"status":"PRECHECK_PASSED","repo_url":"https://github.com/example/repo","requirement_prompt":"actual prompt"}}`)
		case "/uploads/attachment":
			if uploads.Add(1) == 2 {
				http.Error(w, "failed second attachment", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{"code":0,"data":{"url":"https://example.com/attachment"}}`)
		case "/submissions/create":
			creates.Add(1)
			state, err := store.Load(context.Background())
			if err != nil || state.PendingOperation == nil || state.PendingOperation.CreateRequestStarted == nil || !*state.PendingOperation.CreateRequestStarted {
				t.Error("create request arrived before its durable stage event")
			}
			fmt.Fprint(w, `{"code":0,"data":{"id":456,"status":"PENDING_QC"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, _ := New(server.URL, nil)
	c.SetSessionCookies("synthetic", "csrf")
	if _, err := c.SubmitWorkflow(context.Background(), store, p, s, e); err == nil {
		t.Fatal("failed second attachment accepted")
	}
	if creates.Load() != 0 || uploads.Load() != 2 {
		t.Fatal("attachment failure reached create or was automatically retried")
	}
	beforeReset := requests.Load()
	state, err := ResetUncreatedSubmission(context.Background(), store)
	if err != nil || state.Status != WorkflowDryRunPassed || state.PendingOperation != nil || requests.Load() != beforeReset {
		t.Fatalf("local reset failed or reached the platform: %v", err)
	}
	result, err := c.SubmitWorkflow(context.Background(), store, p, s, e)
	if err != nil || result.ID != 456 || uploads.Load() != 4 || creates.Load() != 1 {
		t.Fatalf("explicit resubmit did not upload anew and create once: %v", err)
	}
	if _, err := ResetUncreatedSubmission(context.Background(), store); err == nil {
		t.Fatal("completed submission was reset")
	}
}

func TestWorkflowCreateTimeoutCannotResetLocally(t *testing.T) {
	store, p, s, e := readyWorkflow(t)
	var uploads, creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/precheck/items/2409":
			fmt.Fprint(w, `{"code":0,"data":{"status":"PRECHECK_PASSED","repo_url":"https://github.com/example/repo","requirement_prompt":"actual prompt"}}`)
		case "/uploads/attachment":
			uploads.Add(1)
			fmt.Fprint(w, `{"code":0,"data":{"url":"https://example.com/attachment"}}`)
		case "/submissions/create":
			creates.Add(1)
			time.Sleep(150 * time.Millisecond)
			fmt.Fprint(w, `{"code":0,"data":{"id":789,"status":"PENDING_QC"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, _ := NewWithRetry(server.URL, nil, RetryPolicy{MaxAttempts: 1, RequestTimeout: 50 * time.Millisecond})
	c.SetSessionCookies("synthetic", "csrf")
	if _, err := c.SubmitWorkflow(context.Background(), store, p, s, e); err == nil {
		t.Fatal("timed-out create was accepted")
	}
	state, err := store.Load(context.Background())
	if err != nil || state.PendingOperation == nil || state.PendingOperation.CreateRequestStarted == nil || !*state.PendingOperation.CreateRequestStarted || state.PendingOperation.Outcome != "uncertain" {
		t.Fatalf("ambiguous create stage was not persisted: %v", err)
	}
	if _, err := ResetUncreatedSubmission(context.Background(), store); err == nil {
		t.Fatal("ambiguous create was reset locally")
	}
	if _, err := store.Update(context.Background(), state.Revision, WorkflowAudit{Action: "reconcile", OperationID: state.PendingOperation.ID, Outcome: "reconciled"}, func(next *WorkflowState) error {
		started := false
		next.PendingOperation.CreateRequestStarted = &started
		return nil
	}); err == nil {
		t.Fatal("durable create-stage evidence was reversed")
	}
	if _, err := c.SubmitWorkflow(context.Background(), store, p, s, e); err == nil {
		t.Fatal("ambiguous create was retried")
	}
	if creates.Load() != 1 || uploads.Load() != 2 {
		t.Fatal("ambiguous create caused extra writes")
	}
}

func TestWorkflowLegacyOperationCannotInferUncreated(t *testing.T) {
	store, _, _, _ := readyWorkflow(t)
	ctx := context.Background()
	state, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state, err = store.Update(ctx, state.Revision, WorkflowAudit{Action: "legacy_setup"}, func(next *WorkflowState) error {
		next.Status = WorkflowSubmitting
		next.PendingOperation = &WorkflowOperation{ID: "legacy-op", Kind: "submit", PayloadSHA256: strings.Repeat("a", 64), StartedAt: time.Now().UTC().Format(time.RFC3339), Outcome: "uncertain"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResetUncreatedSubmission(ctx, store); err == nil {
		t.Fatal("legacy missing stage was assumed false")
	}
	if _, err := store.Update(ctx, state.Revision, WorkflowAudit{Action: "reconcile", OperationID: "legacy-op", Outcome: "reconciled"}, func(next *WorkflowState) error {
		started := false
		next.PendingOperation.CreateRequestStarted = &started
		return nil
	}); err == nil {
		t.Fatal("legacy stage was invented locally")
	}
}

func TestWorkflowChangedAttachmentWritesNothing(t *testing.T) {
	store, p, s, e := readyWorkflow(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	c, _ := New(server.URL, nil)
	_ = os.WriteFile(p.Attachments["x_trae_trace"], []byte("actual prompt modified"), 0600)
	_, err := c.SubmitWorkflow(context.Background(), store, p, s, e)
	if err == nil || requests.Load() != 0 {
		t.Fatal("changed evidence reached network")
	}
}

func TestWorkflowAttachmentChangesDuringReadCauseZeroUploads(t *testing.T) {
	store, p, s, e := readyWorkflow(t)
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
			return
		}
		_ = os.WriteFile(p.Attachments["x_trae_trace"], []byte("actual prompt changed during GET"), 0600)
		fmt.Fprint(w, `{"code":0,"data":{"status":"PRECHECK_PASSED","repo_url":"https://github.com/example/repo","requirement_prompt":"actual prompt"}}`)
	}))
	defer server.Close()
	c, _ := New(server.URL, nil)
	_, err := c.SubmitWorkflow(context.Background(), store, p, s, e)
	if err == nil || writes.Load() != 0 {
		t.Fatal("changed attachment reached upload")
	}
}

func TestSubmissionRoundsReadsOnlyANonNegativeInteger(t *testing.T) {
	plan := SubmissionPlan{Payload: map[string]any{"effective_rounds": 100}}
	if rounds, known := SubmissionRounds(plan); !known || rounds != 100 {
		t.Fatalf("rounds=%d known=%v", rounds, known)
	}
	plan.Payload["effective_rounds"] = float64(100)
	if rounds, known := SubmissionRounds(plan); !known || rounds != 100 {
		t.Fatalf("json rounds=%d known=%v", rounds, known)
	}
	for _, value := range []any{nil, "100", 45.5, -1, float64(45.5)} {
		plan.Payload["effective_rounds"] = value
		if rounds, known := SubmissionRounds(plan); known {
			t.Fatalf("%v was accepted as rounds=%d", value, rounds)
		}
	}
}

func TestLocalOnlyMarkableCoversEveryUnsubmittedState(t *testing.T) {
	markable := []WorkflowStatus{WorkflowInitialized, WorkflowProjectSelected, WorkflowPrecheckDraft,
		WorkflowPrecheckSubmitted, WorkflowPrecheckRejected, WorkflowPrecheckPassed, WorkflowTraeRunning,
		WorkflowSessionCaptured, WorkflowSessionVerified, WorkflowArtifactsVerified, WorkflowDryRunPassed}
	for _, status := range markable {
		if !LocalOnlyMarkable(status) {
			t.Fatalf("%s should be markable by hand", status)
		}
		if _, ok := workflowTransitions[status]; !ok {
			t.Fatalf("%s has no transitions", status)
		}
		allowed := false
		for _, next := range workflowTransitions[status] {
			if next == WorkflowLocalOnly {
				allowed = true
			}
		}
		if !allowed {
			t.Fatalf("%s cannot transition to local_only", status)
		}
	}
	// A task that reached the platform keeps the platform's fate.
	for _, status := range []WorkflowStatus{WorkflowSubmitting, WorkflowSubmitted, WorkflowQCPending,
		WorkflowQCRejected, WorkflowQCAccepted, WorkflowClosed, WorkflowLocalOnly} {
		if LocalOnlyMarkable(status) {
			t.Fatalf("%s should not be markable by hand", status)
		}
	}
}

func TestLocalOnlyDecisionFollowsTheCollectionPolicy(t *testing.T) {
	cases := []struct {
		name         string
		rounds       int
		questionType string
		completed    string
		want         bool
	}{
		{"above floor completed", 120, "有效轮数 > 100", "完成", false},
		{"above floor unfinished", 120, "有效轮数 > 100", "部分完成", false},
		{"below floor good effect", 71, "有效轮数 < 100 且 效果好", "完成", true},
		{"below floor completed only", 71, "", "完成", true},
		{"below floor bad effect unfinished", 76, "有效轮数 < 100 且 效果差", "部分完成", false},
		{"below floor unevaluated", 33, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rounds := c.rounds
			state := WorkflowState{Trae: WorkflowTrae{EffectiveRounds: &rounds}}
			plan := SubmissionPlan{Payload: map[string]any{}}
			if c.questionType != "" {
				plan.Payload["question_type"] = c.questionType
			}
			if c.completed != "" {
				plan.Payload["requirement_completed"] = c.completed
			}
			blocked, reason := LocalOnlyDecision(state, plan)
			if blocked != c.want {
				t.Fatalf("blocked=%v want=%v reason=%q", blocked, c.want, reason)
			}
			if blocked && reason == "" {
				t.Fatal("blocked without a reason")
			}
		})
	}
	// A submitted task is never reclassified: its fate belongs to the receipt.
	rounds := 33
	submitted := WorkflowState{Trae: WorkflowTrae{EffectiveRounds: &rounds}, Submission: WorkflowSubmission{ID: 7}}
	if blocked, _ := LocalOnlyDecision(submitted, SubmissionPlan{Payload: map[string]any{"requirement_completed": "完成"}}); blocked {
		t.Fatal("a submitted task was reclassified")
	}
}

func TestSubmitWorkflowKeepsAnIdealShortRunLocal(t *testing.T) {
	store, p, s, e := readyWorkflowWithRounds(t, 45)
	// A finished run below the floor that was implemented well stays on this
	// machine: no platform entry may be created and no write may happen.
	p.Payload["question_type"] = "有效轮数 < 100 且 效果好"
	p.Payload["requirement_completed"] = "完成"
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	c, _ := New(server.URL, nil)
	c.SetSessionCookies("synthetic", "csrf")
	_, err := c.SubmitWorkflow(context.Background(), store, p, s, e)
	if err == nil || !strings.Contains(err.Error(), "仅留存本地") {
		t.Fatalf("ideal short run was not refused: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatalf("network writes=%d", writes.Load())
	}
	state, loadErr := store.Load(context.Background())
	if loadErr != nil || state.Submission.ID != 0 || state.Status != WorkflowDryRunPassed {
		t.Fatalf("state changed: %+v %v", state, loadErr)
	}
}

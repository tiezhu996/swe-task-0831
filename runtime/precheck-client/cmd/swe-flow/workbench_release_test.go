package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	client "example.com/swe-precheck-client"
)

func dryRunFixture(t *testing.T) (string, client.WorkflowState, string) {
	t.Helper()
	root := t.TempDir()
	statePath := filepath.Join(root, "task-state.json")
	tracePath, shotPath, auditPath, planPath := filepath.Join(root, "trace.md"), filepath.Join(root, "shot.png"), filepath.Join(root, "audit.json"), filepath.Join(root, "plan.json")
	for path, content := range map[string]string{tracePath: "trace", shotPath: "shot", auditPath: "audit"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	plan := client.SubmissionPlan{ExecutionPrompt: "题面", Payload: map[string]any{}, Attachments: map[string]string{"x_trae_trace": tracePath, "x_artifact_shot": shotPath}}
	if err := client.SaveLocalJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	planHash, _ := hashFile(planPath)
	auditHash, _ := hashFile(auditPath)
	state := client.WorkflowState{Status: client.WorkflowDryRunPassed, PlanPath: planPath, PlanSHA256: planHash, Precheck: client.WorkflowPrecheck{ItemID: 7, PromptSHA256: client.TextSHA256("题面")}, Trae: client.WorkflowTrae{SessionTokenSHA256: client.TextSHA256("token")}, Submission: client.WorkflowSubmission{SchemaSHA256: client.TextSHA256("schema"), AuditPath: auditPath, AuditSHA256: auditHash}}
	return statePath, state, tracePath
}

func TestReviewGateWaitsForCurrentCodexReview(t *testing.T) {
	statePath, state, tracePath := dryRunFixture(t)
	gate := evaluateReviewGate(statePath, state)
	if gate.Ready || gate.Template == nil || !strings.Contains(gate.Reason, "等待") {
		t.Fatalf("gate without review = %+v", gate)
	}
	review := *gate.Template
	review.SessionLogReviewed, review.OneHumanPrompt, review.SourceDiffReviewed, review.RubricEvidenceReviewed = true, true, true, true
	review.Notes = "核对了轨迹首条人工消息、Session 日志、源码差异与逐项 Rubric 输出"
	if err := client.SaveLocalJSON(gate.Path, review); err != nil {
		t.Fatal(err)
	}
	if gate = evaluateReviewGate(statePath, state); !gate.Ready {
		t.Fatalf("current review not accepted: %+v", gate)
	}
	view := workbenchTask{SessionSHA256: "x", Artifacts: map[string]bool{"x_trae_trace": true}}
	if stage := workbenchStage(view, state, gate, nil); stage != "ready" {
		t.Fatalf("stage=%s", stage)
	}
	if err := os.WriteFile(tracePath, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if gate = evaluateReviewGate(statePath, state); gate.Ready {
		t.Fatal("review accepted after trace changed")
	}
	if stage := workbenchStage(view, state, gate, nil); stage != "review" {
		t.Fatalf("stale review stage=%s", stage)
	}
	state.Submission.ID = 12
	if gate = evaluateReviewGate(statePath, state); gate.Ready {
		t.Fatal("existing submission must never be ready to create again")
	}
}

func dryRunTask(t *testing.T) (*workbenchServer, string) {
	t.Helper()
	root := t.TempDir()
	path := setupWorkbenchTask(t, root, "dry", "https://github.com/example/repo", "冻结题面")
	store := client.NewWorkflowStore(path)
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	steps := []client.WorkflowStatus{client.WorkflowPrecheckDraft, client.WorkflowPrecheckSubmitted, client.WorkflowPrecheckPassed, client.WorkflowTraeRunning, client.WorkflowSessionCaptured, client.WorkflowSessionVerified, client.WorkflowArtifactsVerified, client.WorkflowDryRunPassed}
	for _, next := range steps {
		state, err = store.Update(context.Background(), state.Revision, client.WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *client.WorkflowState) error {
			s.Status = next
			s.PlanSHA256 = client.TextSHA256("plan")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.host = "127.0.0.1:8765"
	return w, path
}

func postSubmit(w *workbenchServer, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "http://127.0.0.1:8765/api/tasks/"+workbenchTaskKey(path)+"/submit", strings.NewReader(body))
	req.Header.Set("Origin", "http://127.0.0.1:8765")
	req.Header.Set("X-CSRF-Token", w.csrf)
	response := httptest.NewRecorder()
	w.handler().ServeHTTP(response, req)
	return response
}

func TestSubmitEndpointRefusesUnreviewedOrStaleMaterials(t *testing.T) {
	w, path := dryRunTask(t)
	called := false
	w.runSubmit = func(context.Context, string, string) error { called = true; return nil }
	if response := postSubmit(w, path, `{"confirm":true,"plan_sha256":"stale"}`); response.Code != 409 || !strings.Contains(response.Body.String(), "过期") {
		t.Fatalf("stale plan status=%d body=%s", response.Code, response.Body.String())
	}
	body := fmt.Sprintf(`{"confirm":true,"plan_sha256":%q}`, client.TextSHA256("plan"))
	for range 2 {
		// A refused attempt must release its slot so the next one is judged on materials again.
		if response := postSubmit(w, path, body); response.Code != 409 || !strings.Contains(response.Body.String(), "尚未审核完成") {
			t.Fatalf("unreviewed status=%d body=%s", response.Code, response.Body.String())
		}
	}
	if response := postSubmit(w, path, `{"confirm":false}`); response.Code != 400 {
		t.Fatalf("unconfirmed status=%d", response.Code)
	}
	if called {
		t.Fatal("submit ran without a completed review")
	}
}

func TestSubmitJobReportsPlatformReceiptOrFailure(t *testing.T) {
	w, path := dryRunTask(t)
	key := workbenchTaskKey(path)
	w.runSubmit = func(ctx context.Context, statePath, tools string) error {
		return fmt.Errorf("提交审核未通过：schema_changed；无上传、无创建")
	}
	w.runSubmitJob(key, path, "tools", workbenchSubmitJob{State: "running", StartedAt: "2026-10-07T00:00:00Z"})
	job := w.submitJob(key, path)
	if job == nil || job.State != "failed" || !strings.Contains(job.Message, "schema_changed") {
		t.Fatalf("failed job=%+v", job)
	}
	if view := w.taskView(context.Background(), path); view.Stage != "execute" || view.SubmitJob == nil {
		t.Fatalf("failed view stage=%s job=%+v", view.Stage, view.SubmitJob)
	}
	w.runSubmit = func(ctx context.Context, statePath, tools string) error {
		store := client.NewWorkflowStore(statePath)
		state, err := store.Load(ctx)
		if err != nil {
			return err
		}
		_, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "fixture_submit", Outcome: "success"}, func(s *client.WorkflowState) error {
			s.Submission.ID, s.Submission.Status, s.Status = 1701, "PENDING_QC", client.WorkflowSubmitting
			return nil
		})
		if err != nil {
			return err
		}
		state, _ = store.Load(ctx)
		_, err = store.Advance(ctx, state.Revision, client.WorkflowSubmitted, client.WorkflowAudit{Action: "fixture_receipt", Outcome: "success"})
		return err
	}
	w.runSubmitJob(key, path, "tools", workbenchSubmitJob{State: "running", StartedAt: "2026-10-07T00:00:00Z"})
	restarted, err := newWorkbenchServer(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	if err != nil {
		t.Fatal(err)
	}
	job = restarted.submitJob(key, path)
	if job == nil || job.State != "succeeded" || job.SubmissionID != 1701 || job.SubmissionStatus != "PENDING_QC" {
		t.Fatalf("persisted job=%+v", job)
	}
	if view := restarted.taskView(context.Background(), path); view.Stage != "submitted" || view.SubmissionID != 1701 {
		t.Fatalf("submitted view=%+v", view)
	}
}

func TestInterruptedSubmitIsShownAsUncertain(t *testing.T) {
	w, path := dryRunTask(t)
	if err := client.SaveLocalJSON(submitResultPath(path), workbenchSubmitJob{State: "running", StartedAt: "2026-10-07T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	view := w.taskView(context.Background(), path)
	if view.SubmitJob == nil || view.SubmitJob.State != "uncertain" || view.Stage == "submitting" {
		t.Fatalf("interrupted view stage=%s job=%+v", view.Stage, view.SubmitJob)
	}
}

func TestPlatformGateReportsQuotaAndKeepsLastAnswer(t *testing.T) {
	limited := true
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if fail || r.URL.Path != "/submissions/form-schema" {
			http.Error(out, "down", 503)
			return
		}
		fmt.Fprintf(out, `{"code":0,"data":{"schema_version":"v1","submission_limited":%t,"fields":[]}}`, limited)
	}))
	defer server.Close()
	w, err := newWorkbenchServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w.newClient = func(context.Context) (*client.Client, error) { return client.New(server.URL, nil) }
	gate := w.platformGateStatus(context.Background(), true)
	if !gate.Known || gate.Open || !gate.Limited || !strings.Contains(platformClosedMessage(gate), "额度") {
		t.Fatalf("limited gate=%+v", gate)
	}
	limited = false
	if gate = w.platformGateStatus(context.Background(), false); !gate.Limited {
		t.Fatal("cached answer should be reused inside the TTL")
	}
	if gate = w.platformGateStatus(context.Background(), true); !gate.Open {
		t.Fatalf("reopened gate=%+v", gate)
	}
	fail = true
	if gate = w.platformGateStatus(context.Background(), true); !gate.Known || !gate.Open || gate.Error == "" {
		t.Fatalf("failed refresh should keep last real answer: %+v", gate)
	}
}

func TestModelSetRewritesBindingAndReopensReview(t *testing.T) {
	root := t.TempDir()
	path := setupWorkbenchTask(t, root, "model", "https://github.com/example/repo", "冻结题面")
	planPath := filepath.Join(filepath.Dir(path), "submission-plan.json")
	plan := client.SubmissionPlan{ExecutionPrompt: "冻结题面", Payload: map[string]any{"repo_url": "https://github.com/example/repo", "commit_ref": strings.Repeat("a", 40), "primary_language": "Go", "precheck_item_id": 7, "seed_model": "Seed-Evolving Max"}, Attachments: map[string]string{}}
	if err := client.SaveLocalJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	store := client.NewWorkflowStore(path)
	ctx := context.Background()
	state, _ := store.Load(ctx)
	for _, next := range []client.WorkflowStatus{client.WorkflowPrecheckDraft, client.WorkflowPrecheckSubmitted, client.WorkflowPrecheckPassed, client.WorkflowTraeRunning, client.WorkflowSessionCaptured, client.WorkflowSessionVerified, client.WorkflowArtifactsVerified} {
		var err error
		if state, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *client.WorkflowState) error {
			s.Status = next
			s.Trae.Model = "Seed-Evolving Max"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.BindPlan(ctx, state.Revision, planPath); err != nil {
		t.Fatal(err)
	}
	if err := runOperation("model-set", []string{"-state", path, "-model", "Seed-Evolving"}); err != nil {
		t.Fatal(err)
	}
	state, _ = store.Load(ctx)
	saved, _ := client.ReadSubmissionPlan(planPath)
	hash, _ := hashFile(planPath)
	if state.Status != client.WorkflowSessionVerified || state.Trae.Model != "Seed-Evolving" || saved.Payload["seed_model"] != "Seed-Evolving" || state.PlanSHA256 != hash {
		t.Fatalf("status=%s model=%q plan=%v bound=%v", state.Status, state.Trae.Model, saved.Payload["seed_model"], state.PlanSHA256 == hash)
	}
	if err := runOperation("model-set", []string{"-state", path, "-model", "Seed-Evolving"}); err != nil {
		t.Fatalf("model-set must be repeatable: %v", err)
	}
}

func TestHandoffSubmitToggleChoosesLocalOrPlatform(t *testing.T) {
	state := client.WorkflowState{TaskID: "one", Status: client.WorkflowSessionVerified}
	local := manualCodexHandoffMode("/workflow/one/task-state.json", state, false)
	remote := manualCodexHandoffMode("/workflow/one/task-state.json", state, true)
	if !strings.Contains(local, "留在本地") || strings.Contains(local, "submit -state") {
		t.Fatal("local handoff must stop before any platform write")
	}
	if !strings.Contains(remote, "直接提交") || !strings.Contains(remote, "submission_limited") {
		t.Fatal("platform handoff must submit and fall back to local when the platform is closed")
	}
	for _, text := range []string{local, remote} {
		if !strings.Contains(text, "Seed-Evolving") {
			t.Fatal("handoff must carry the model naming rule")
		}
	}
}

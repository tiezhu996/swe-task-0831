package main

import (
	"context"
	"encoding/json"
	client "example.com/swe-precheck-client"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixturePassedTask(t *testing.T, root string) (string, client.WorkflowState) {
	t.Helper()
	path := setupWorkbenchTask(t, root, "after-run", "https://github.com/example/repo", "冻结题面")
	store := client.NewWorkflowStore(path)
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []client.WorkflowStatus{client.WorkflowPrecheckDraft, client.WorkflowPrecheckSubmitted, client.WorkflowPrecheckPassed} {
		state, err = store.Update(context.Background(), state.Revision, client.WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *client.WorkflowState) error { s.Precheck.ItemID = 99; s.Status = next; return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "PROMPT.md"), []byte("冻结题面\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path, state
}

func TestManualEvidenceAcceptsChangedSourceWithoutInventingExecution(t *testing.T) {
	root := t.TempDir()
	path, state := fixturePassedTask(t, root)
	// Copy a real baseline into this fixture's already-bound source directory.
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-qm", "baseline"}} {
		if data, err := exec.Command("git", append([]string{"-C", state.Workspace}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v", data, err)
		}
	}
	head, err := exec.Command("git", "-C", state.Workspace, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	store := client.NewWorkflowStore(path)
	state, err = store.Update(context.Background(), state.Revision, client.WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *client.WorkflowState) error {
		s.Project.BaselineCommit = strings.TrimSpace(string(head))
		s.PlanPath = filepath.Join(filepath.Dir(path), "submission-plan.json")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SaveLocalJSON(state.PlanPath, client.SubmissionPlan{ExecutionPrompt: "冻结题面", Payload: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	receipt := sourcePreparation{SchemaVersion: 1, PrecheckID: 99, RepoURL: state.Project.RepoURL, BaselineCommit: state.Project.BaselineCommit, PromptSHA256: state.Precheck.PromptSHA256, Workspace: state.Workspace}
	if err := client.SaveLocalJSON(filepath.Join(filepath.Dir(path), "source-preparation.json"), receipt); err != nil {
		t.Fatal(err)
	}
	// Commit and leave an additional dirty change, as a model is allowed to do.
	if err := os.WriteFile(filepath.Join(state.Workspace, "model.go"), []byte("package example\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "model.go"}, {"-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "model change"}} {
		if err := exec.Command("git", append([]string{"-C", state.Workspace}, args...)...).Run(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(state.Workspace, "model.go"), []byte("package example\n// finished change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(state.Workspace, "model.go"))
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	collected, err := w.prepareManualCapture(context.Background(), path, state)
	if err != nil || collected.Status != client.WorkflowTraeRunning || collected.Trae.ExecutionMode != "manual" || collected.Trae.ExecutionStartedAt != "" || collected.Trae.Model != "" {
		t.Fatalf("state=%+v err=%v", collected, err)
	}
	after, _ := os.ReadFile(filepath.Join(state.Workspace, "model.go"))
	if string(before) != string(after) {
		t.Fatal("model source was modified")
	}
	repeated, err := w.prepareManualCapture(context.Background(), path, collected)
	if err != nil || repeated.Revision != collected.Revision {
		t.Fatalf("non-idempotent collection err=%v", err)
	}
	// A legacy task can have a model recorded after its empty plan was made.
	schema := client.FormSchema{Version: "v1", Fields: []client.FormField{{Key: "repo_url", Type: "url"}, {Key: "precheck_item_id", Type: "number"}, {Key: "commit_ref", Type: "text"}, {Key: "primary_language", Type: "text"}, {Key: "seed_model", Type: "text"}}}
	plan, err := client.PrepareWorkflowPlan(collected, client.SubmissionPlan{Payload: map[string]any{}}, schema, "冻结题面")
	if err != nil {
		t.Fatal(err)
	}
	plan.SchemaSHA256 = client.FormSchemaSHA256(schema)
	if err := client.SaveLocalJSON(collected.PlanPath, plan); err != nil {
		t.Fatal(err)
	}
	schemaPath := filepath.Join(filepath.Dir(path), "schema.json")
	if err := client.SaveLocalJSON(schemaPath, schema); err != nil {
		t.Fatal(err)
	}
	collected, err = store.Update(context.Background(), collected.Revision, client.WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *client.WorkflowState) error {
		s.Submission.SchemaPath = schemaPath
		s.Submission.SchemaSHA256 = plan.SchemaSHA256
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	collected, err = store.BindPlan(context.Background(), collected.Revision, collected.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	collected, err = store.Update(context.Background(), collected.Revision, client.WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *client.WorkflowState) error { s.Trae.Model = "Recorded model"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	collected, err = w.prepareManualCapture(context.Background(), path, collected)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := client.ReadSubmissionPlan(collected.PlanPath)
	if err != nil || updated.Payload["seed_model"] != "Recorded model" || collected.Trae.ExecutionStartedAt != "" {
		t.Fatalf("legacy model not synchronized: err=%v", err)
	}
	receipt.PrecheckID = 100
	_ = client.SaveLocalJSON(filepath.Join(filepath.Dir(path), "source-preparation.json"), receipt)
	if _, err := w.prepareManualCapture(context.Background(), path, collected); err == nil {
		t.Fatal("wrong baseline receipt accepted")
	}
}

func TestManualHandoffTargetsPostRunAndNeverExposesSession(t *testing.T) {
	state := client.WorkflowState{TaskID: "one", Status: client.WorkflowPrecheckPassed, Workspace: "/source/one", Project: client.WorkflowProject{RepoURL: "https://github.com/example/repo", BaselineCommit: strings.Repeat("a", 40)}, Precheck: client.WorkflowPrecheck{ItemID: 99, PromptSHA256: client.TextSHA256("冻结题面")}}
	text := manualCodexHandoff("/workflow/one/task-state.json", state)
	for _, want := range []string{"我已在 Trae 输入冻结题面并执行完毕", "用户只负责补充 Session 和原生轨迹", "核实实际模型及有效轮数", "源码已经变动是执行产物", "禁止重新输入题面", "/workflow/one/task-state.json"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q", want)
		}
	}
	state.Trae.SessionTokenSHA256 = client.TextSHA256("private")
	state.Verification.Artifacts = map[string]client.WorkflowArtifact{"x_trae_trace": {}}
	text = manualCodexHandoff("/workflow/one/task-state.json", state)
	if strings.Contains(text, "用户待补材料") || !strings.Contains(text, "不要求用户重复提供") {
		t.Fatal("already supplied evidence requested again")
	}
}

func TestFormRequiresFieldSpecificConfirmedGrant(t *testing.T) {
	root := t.TempDir()
	path, state := fixturePassedTask(t, root)
	schema := client.FormSchema{Version: "v1", Fields: []client.FormField{{Key: "repo_url", Type: "url"}, {Key: "precheck_item_id", Type: "number"}, {Key: "commit_ref", Type: "text"}, {Key: "primary_language", Type: "text"}, {Key: "product_result", Type: "textarea"}, {Key: "title", Type: "text"}, {Key: "trae_session_id", Type: "text"}}}
	plan, err := client.PrepareWorkflowPlan(state, client.SubmissionPlan{Payload: map[string]any{}}, schema, "冻结题面")
	if err != nil {
		t.Fatal(err)
	}
	plan.SchemaSHA256 = client.FormSchemaSHA256(schema)
	planPath := filepath.Join(filepath.Dir(path), "submission-plan.json")
	if err := client.SaveLocalJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	store := client.NewWorkflowStore(path)
	state, err = store.Update(context.Background(), state.Revision, client.WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *client.WorkflowState) error { s.Submission.SchemaSHA256 = plan.SchemaSHA256; return nil })
	if err != nil {
		t.Fatal(err)
	}
	state, err = store.BindPlan(context.Background(), state.Revision, planPath)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(out).Encode(map[string]any{"code": 0, "data": schema})
	}))
	defer server.Close()
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.host = "127.0.0.1:8765"
	w.newClient = func(context.Context) (*client.Client, error) { return client.New(server.URL, nil) }
	w.runFlow = func(ctx context.Context, args ...string) ([]byte, error) {
		fresh, err := store.Load(ctx)
		if err != nil {
			return nil, err
		}
		_, err = store.BindPlan(ctx, fresh.Revision, planPath)
		return []byte(`{}`), err
	}
	request := func(method, action string, body any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		req := httptest.NewRequest(method, "http://127.0.0.1:8765/api/tasks/"+workbenchTaskKey(path)+"/"+action, strings.NewReader(string(data)))
		req.Header.Set("Origin", "http://127.0.0.1:8765")
		req.Header.Set("X-CSRF-Token", w.csrf)
		out := httptest.NewRecorder()
		w.handler().ServeHTTP(out, req)
		return out
	}
	response := request("GET", "form", nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"locked":true`) || !strings.Contains(response.Body.String(), `"editable":true`) {
		t.Fatalf("form=%s", response.Body.String())
	}
	write := func(field, token, hash string) *httptest.ResponseRecorder {
		return request("PUT", "form", map[string]any{"values": map[string]any{field: "实测结果"}, "unlock_tokens": map[string]string{field: token}, "plan_sha256": hash})
	}
	if response := write("product_result", "", state.PlanSHA256); response.Code != 400 {
		t.Fatalf("unguarded write status=%d %s", response.Code, response.Body.String())
	}
	unlock := func(field string, confirm bool, hash string) *httptest.ResponseRecorder {
		return request("POST", "form/unlock", map[string]any{"field": field, "confirm": confirm, "plan_sha256": hash})
	}
	for _, field := range []string{"repo_url", "trae_session_id"} {
		if response := unlock(field, true, state.PlanSHA256); response.Code != 400 {
			t.Fatalf("protected %s status=%d", field, response.Code)
		}
	}
	if response := unlock("product_result", false, state.PlanSHA256); response.Code != 400 {
		t.Fatal("confirmation skipped")
	}
	response = unlock("product_result", true, state.PlanSHA256)
	var grant struct {
		Token string `json:"unlock_token"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &grant)
	if response.Code != 200 || grant.Token == "" {
		t.Fatalf("unlock=%s", response.Body.String())
	}
	if response := write("title", grant.Token, state.PlanSHA256); response.Code != 400 {
		t.Fatal("grant used for another field")
	}
	record := w.fieldUnlocks[grant.Token]
	record.Expires = time.Now().Add(-time.Minute)
	w.fieldUnlocks[grant.Token] = record
	if response := write("product_result", grant.Token, state.PlanSHA256); response.Code != 400 {
		t.Fatal("expired grant accepted")
	}
	response = unlock("product_result", true, state.PlanSHA256)
	_ = json.Unmarshal(response.Body.Bytes(), &grant)
	if response := write("product_result", grant.Token, state.PlanSHA256); response.Code != 200 {
		t.Fatalf("confirmed write: %d %s", response.Code, response.Body.String())
	}
	fresh, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response := write("product_result", grant.Token, fresh.PlanSHA256); response.Code != 400 {
		t.Fatal("consumed grant reused")
	}
	if response := unlock("product_result", true, state.PlanSHA256); response.Code != 409 {
		t.Fatal("stale plan unlocked")
	}
	stored, err := client.ReadSubmissionPlan(planPath)
	if err != nil || fmt.Sprint(stored.Payload["product_result"]) != "实测结果" {
		t.Fatalf("stored=%v err=%v", stored.Payload, err)
	}
}

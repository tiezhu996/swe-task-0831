package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	client "example.com/swe-precheck-client"
)

func TestMain(m *testing.M) {
	private, err := os.MkdirTemp("", "swe-workbench-private-test-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("SWE_WORKBENCH_STATE_DIR", private)
	_ = os.Setenv("SWE_PRECHECK_DESIGN_DIR", filepath.Join(private, "precheck-designs"))
	code := m.Run()
	_ = os.RemoveAll(private)
	os.Exit(code)
}

func setupWorkbenchTask(t *testing.T, root, folder, repo, prompt string) string {
	t.Helper()
	dir := filepath.Join(root, "tasks", folder)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "sources", folder)
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "task-state.json")
	store := client.NewWorkflowStore(path)
	state, err := store.Init(context.Background(), folder, workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(context.Background(), state.Revision, client.WorkflowAudit{Action: "project_bind", Outcome: "success"}, func(s *client.WorkflowState) error {
		s.Project = client.WorkflowProject{RepoURL: repo, BaselineCommit: strings.Repeat("a", 40), Language: "Go"}
		s.Precheck.PromptSHA256 = client.TextSHA256(prompt)
		s.Status = client.WorkflowProjectSelected
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestWorkbenchCreatesCleanTaskOutsideSourceTree(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "sources", "new")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-qm", "baseline"}} {
		cmd := exec.Command("git", append([]string{"-C", workspace}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	head, err := exec.Command("git", "-C", workspace, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	w, _ := newWorkbenchServer(root)
	path, err := w.createTaskFiles(context.Background(), newTaskRequest{TaskDir: "tasks/new", Workspace: "sources/new", TaskID: "new", RepoURL: "https://github.com/example/repo", Baseline: strings.TrimSpace(string(head)), Language: "Go", Prompt: "冻结的需求题面"}, false)
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.NewWorkflowStore(path).Load(context.Background())
	if err != nil || state.Status != client.WorkflowProjectSelected {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if text, err := os.ReadFile(filepath.Join(filepath.Dir(path), "PROMPT.md")); err != nil || string(text) != "冻结的需求题面\n" {
		t.Fatalf("prompt=%q err=%v", text, err)
	}
}

func TestWorkbenchKeepsBrokenTaskReadOnly(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "task-state.json")
	if err := os.WriteFile(path, []byte(`{"task_id":"old","status":"qc_pending","project":{"repo_url":"https://github.com/o/r"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	path, _ = filepath.EvalSymlinks(path)
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.host = "127.0.0.1:8765"
	req := httptest.NewRequest("GET", "http://127.0.0.1:8765/api/tasks", nil)
	response := httptest.NewRecorder()
	w.handler().ServeHTTP(response, req)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"integrity_error":true`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	req = httptest.NewRequest("POST", "http://127.0.0.1:8765/api/tasks/"+workbenchTaskKey(path)+"/authorize", strings.NewReader(`{"confirm":true}`))
	req.Header.Set("Origin", "http://127.0.0.1:8765")
	req.Header.Set("X-CSRF-Token", w.csrf)
	response = httptest.NewRecorder()
	w.handler().ServeHTTP(response, req)
	if response.Code != 409 {
		t.Fatalf("broken task mutation status=%d", response.Code)
	}
}

func TestWorkbenchTaskViewExposesRecordedSampleFields(t *testing.T) {
	root := t.TempDir()
	path := setupWorkbenchTask(t, root, "sample", "https://github.com/example/repo", "冻结题面")
	planPath := filepath.Join(filepath.Dir(path), "submission-plan.json")
	plan := client.SubmissionPlan{SchemaVersion: "v1", ExecutionPrompt: "冻结题面", Payload: map[string]any{
		"question_type":            "有效轮数 < 100 且 效果好",
		"x_is_more_than_two_hours": "否",
	}}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	// Without a bound plan both markers stay empty rather than guessing.
	if view := w.taskView(context.Background(), path); view.QuestionType != "" || view.MoreThanTwoHours != "" {
		t.Fatalf("unbound plan produced type=%q hours=%q", view.QuestionType, view.MoreThanTwoHours)
	}
	store := client.NewWorkflowStore(path)
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(context.Background(), state.Revision, client.WorkflowAudit{Action: "plan_bind", Outcome: "success"}, func(s *client.WorkflowState) error {
		s.PlanPath = planPath
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	view := w.taskView(context.Background(), path)
	if view.QuestionType != "有效轮数 < 100 且 效果好" || view.MoreThanTwoHours != "否" {
		t.Fatalf("type=%q hours=%q", view.QuestionType, view.MoreThanTwoHours)
	}
}

func TestWorkbenchSyncMatchesFullPromptAndKeepsDuplicateRuns(t *testing.T) {
	root := t.TempDir()
	repo, prompt := "https://github.com/example/repo", "冻结的完整题面"
	first := setupWorkbenchTask(t, root, "first", repo, prompt)
	second := setupWorkbenchTask(t, root, "second", repo, prompt)
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		out.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/precheck/items":
			fmt.Fprint(out, `{"code":0,"data":{"items":[{"id":11,"repo_url":"https://github.com/example/repo","prompt_excerpt":"冻结的完整题面"},{"id":12,"repo_url":"https://github.com/example/repo","prompt_excerpt":"冻结的完整题面"}],"page":1,"page_size":100,"total":2}}`)
		case "/precheck/items/11":
			fmt.Fprint(out, `{"code":0,"data":{"id":11,"repo_url":"https://github.com/example/repo","requirement_prompt":"冻结的完整题面"}}`)
		case "/precheck/items/12":
			fmt.Fprint(out, `{"code":0,"data":{"id":12,"repo_url":"https://github.com/example/repo","requirement_prompt":"只是前半句；后半句不同"}}`)
		default:
			http.NotFound(out, r)
		}
	}))
	defer server.Close()
	w, _ := newWorkbenchServer(root)
	w.newClient = func(context.Context) (*client.Client, error) { return client.New(server.URL, nil) }
	response := httptest.NewRecorder()
	w.handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/prechecks?refresh=1", nil))
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var data struct {
		Items []struct {
			ID      int      `json:"id"`
			Matches []string `json:"matches"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Items) != 2 || len(data.Items[0].Matches) != 2 || len(data.Items[1].Matches) != 0 {
		t.Fatalf("matches=%+v", data.Items)
	}
	for _, key := range []string{workbenchTaskKey(first), workbenchTaskKey(second)} {
		if data.Items[0].Matches[0] != key && data.Items[0].Matches[1] != key {
			t.Fatalf("missing independent task %s", key)
		}
	}
}

func TestWorkbenchSyncReadsEveryPrecheckPage(t *testing.T) {
	root := t.TempDir()
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/precheck/items" {
			http.NotFound(out, r)
			return
		}
		pages++
		page := r.URL.Query().Get("page")
		items := make([]map[string]any, 0, 100)
		start, end := 1, 100
		if page == "2" {
			start, end = 101, 101
		}
		for id := start; id <= end; id++ {
			items = append(items, map[string]any{"id": id, "repo_url": "https://github.com/example/repo"})
		}
		pageNumber := 1
		if page == "2" {
			pageNumber = 2
		}
		_ = json.NewEncoder(out).Encode(map[string]any{"code": 0, "data": map[string]any{"items": items, "page": pageNumber, "page_size": 100, "total": 101}})
	}))
	defer server.Close()
	w, _ := newWorkbenchServer(root)
	w.newClient = func(context.Context) (*client.Client, error) { return client.New(server.URL, nil) }
	response := httptest.NewRecorder()
	w.handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/prechecks?refresh=1", nil))
	if response.Code != 200 || pages != 2 || !strings.Contains(response.Body.String(), `"total":101`) {
		t.Fatalf("pages=%d status=%d body=%s", pages, response.Code, response.Body.String())
	}
}

func TestWorkbenchDoesNotCreateTaskForConsumedPrecheck(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		fmt.Fprint(out, `{"code":0,"data":{"id":2409,"repo_url":"https://github.com/example/repo","requirement_prompt":"题面","status":"PRECHECK_CONSUMED"}}`)
	}))
	defer server.Close()
	w, _ := newWorkbenchServer(root)
	w.host = "127.0.0.1:8765"
	w.newClient = func(context.Context) (*client.Client, error) { return client.New(server.URL, nil) }
	req := httptest.NewRequest("POST", "http://127.0.0.1:8765/api/prechecks/2409/import", strings.NewReader(`{"task_dir":"tasks/new","workspace":"sources/new","task_id":"new","baseline":"abc","language":"Go"}`))
	req.Header.Set("Origin", "http://127.0.0.1:8765")
	req.Header.Set("X-CSRF-Token", w.csrf)
	response := httptest.NewRecorder()
	w.handler().ServeHTTP(response, req)
	if response.Code != 409 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "new")); !os.IsNotExist(err) {
		t.Fatal("consumed precheck created a task")
	}
}

func TestWorkbenchFormNeverReturnsSessionValue(t *testing.T) {
	root := t.TempDir()
	path := setupWorkbenchTask(t, root, "form", "https://github.com/example/repo", "题面")
	schema := client.FormSchema{Version: "one"}
	schema.Fields = []client.FormField{{Key: "trae_session_id", Label: "Session", Type: "text"}, {Key: "product_result", Label: "结果", Type: "textarea"}}
	planPath := filepath.Join(filepath.Dir(path), "submission-plan.json")
	plan := client.SubmissionPlan{ExecutionPrompt: "题面", Payload: map[string]any{"trae_session_id": "PRIVATE_SESSION_VALUE", "product_result": "已核验"}}
	if err := client.SaveLocalJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	store := client.NewWorkflowStore(path)
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(context.Background(), state.Revision, client.WorkflowAudit{Action: "test_plan", Outcome: "success"}, func(s *client.WorkflowState) error {
		s.PlanPath = planPath
		s.PlanSHA256, _ = hashFile(planPath)
		s.Submission.SchemaSHA256 = client.FormSchemaSHA256(schema)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	version := "one"
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(out, `{"code":0,"data":{"schema_version":%q,"fields":[{"field_key":"trae_session_id","label":"Session","type":"text"},{"field_key":"product_result","label":"结果","type":"textarea"}]}}`, version)
	}))
	defer server.Close()
	w, _ := newWorkbenchServer(root)
	w.newClient = func(context.Context) (*client.Client, error) { return client.New(server.URL, nil) }
	response := httptest.NewRecorder()
	w.handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/tasks/"+workbenchTaskKey(path)+"/form", nil))
	if response.Code != 200 || strings.Contains(response.Body.String(), "PRIVATE_SESSION_VALUE") || !strings.Contains(response.Body.String(), "已核验") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	version = "two"
	response = httptest.NewRecorder()
	w.handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/tasks/"+workbenchTaskKey(path)+"/form", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"schema_changed":true`) || !strings.Contains(response.Body.String(), `"editable":false`) {
		t.Fatalf("schema drift status=%d body=%s", response.Code, response.Body.String())
	}
	req := httptest.NewRequest("PUT", "/api/tasks/"+workbenchTaskKey(path)+"/form", strings.NewReader(`{"values":{"product_result":"changed"}}`))
	req.Header.Set("Origin", "http://example.com")
	req.Host = "example.com"
	req.Header.Set("X-CSRF-Token", w.csrf)
	response = httptest.NewRecorder()
	w.handler().ServeHTTP(response, req)
	if response.Code != 409 {
		t.Fatalf("schema drift mutation status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestWorkbenchTraeStartRequiresManualMode(t *testing.T) {
	root := t.TempDir()
	path := setupWorkbenchTask(t, root, "manual", "https://github.com/example/repo", "手动执行题面")
	store := client.NewWorkflowStore(path)
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []client.WorkflowStatus{client.WorkflowPrecheckDraft, client.WorkflowPrecheckSubmitted, client.WorkflowPrecheckPassed} {
		state, err = store.Update(context.Background(), state.Revision, client.WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *client.WorkflowState) error {
			s.Status = next
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
	called := false
	w.runFlow = func(_ context.Context, args ...string) ([]byte, error) {
		called = true
		if strings.Join(args, " ") != "trae-start -state "+path+" -model Seed -mode manual" {
			t.Fatalf("unexpected flow args: %v", args)
		}
		return []byte(`{"recorded":true}`), nil
	}
	request := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://127.0.0.1:8765/api/tasks/"+workbenchTaskKey(path)+"/trae-start", strings.NewReader(body))
		req.Header.Set("Origin", "http://127.0.0.1:8765")
		req.Header.Set("X-CSRF-Token", w.csrf)
		response := httptest.NewRecorder()
		w.handler().ServeHTTP(response, req)
		return response
	}
	if response := request(`{"model":"Seed","confirm":true}`); response.Code != 400 || called {
		t.Fatalf("missing manual mode status=%d called=%v body=%s", response.Code, called, response.Body.String())
	}
	if response := request(`{"mode":"manual","model":"Seed","confirm":true}`); response.Code != 200 || !called {
		t.Fatalf("manual mode status=%d called=%v body=%s", response.Code, called, response.Body.String())
	}
}

func TestWorkbenchAuthorizationInvalidatedByEvidenceChange(t *testing.T) {
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
	approval, err := buildWorkbenchApproval(statePath, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SaveLocalJSON(filepath.Join(root, "workbench-approval.json"), approval); err != nil {
		t.Fatal(err)
	}
	if _, err := validateWorkbenchApproval(statePath, state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracePath, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateWorkbenchApproval(statePath, state); err == nil {
		t.Fatal("stale evidence was accepted")
	}
}

func TestWorkbenchSubmitRequiresCurrentCodexReview(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "task-state.json")
	reviewPath := filepath.Join(root, "workbench-review.json")
	approval := workbenchApproval{PlanSHA256: client.TextSHA256("plan"), PromptSHA256: client.TextSHA256("prompt"), TraceSHA256: client.TextSHA256("trace")}
	review := workbenchReview{SchemaVersion: 1, PlanSHA256: approval.PlanSHA256, PromptSHA256: approval.PromptSHA256, TraceSHA256: approval.TraceSHA256, SessionLogReviewed: true, OneHumanPrompt: true, SourceDiffReviewed: true, Notes: "实际核对了日志和源码差异"}
	if err := client.SaveLocalJSON(reviewPath, review); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkbenchReview(statePath, approval, reviewPath); err == nil {
		t.Fatal("missing rubric review was accepted")
	}
	review.RubricEvidenceReviewed = true
	review.TraceSHA256 = client.TextSHA256("other trace")
	if err := client.SaveLocalJSON(reviewPath, review); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkbenchReview(statePath, approval, reviewPath); err == nil {
		t.Fatal("stale trace review was accepted")
	}
}

func TestWorkbenchRejectsCrossOriginMutation(t *testing.T) {
	w, _ := newWorkbenchServer(t.TempDir())
	w.host = "127.0.0.1:8765"
	req := httptest.NewRequest("POST", "http://127.0.0.1:8765/api/tasks", strings.NewReader(`{}`))
	req.Header.Set("Origin", "http://other.example")
	req.Header.Set("X-CSRF-Token", w.csrf)
	response := httptest.NewRecorder()
	w.handler().ServeHTTP(response, req)
	if response.Code != 403 {
		t.Fatalf("cross-origin status=%d", response.Code)
	}
}

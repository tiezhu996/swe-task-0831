package main

import (
	"bytes"
	"context"
	"encoding/json"
	client "example.com/swe-precheck-client"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sessionPost(w *workbenchServer, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "http://127.0.0.1:8765/api/tasks/"+key+"/session", strings.NewReader(body))
	req.Header.Set("Origin", "http://127.0.0.1:8765")
	req.Header.Set("X-CSRF-Token", w.csrf)
	response := httptest.NewRecorder()
	w.handler().ServeHTTP(response, req)
	return response
}

func savedSessionToken() string {
	return strings.Repeat("a", 16) + ":" + strings.Repeat("b", 32) + "_" + strings.Repeat("c", 24) + "." + strings.Repeat("d", 24) + "." + strings.Repeat("e", 24) + ":TraeCode CN.3.3.90.no_sid.no_ppe.T(2026/10/5 20:14:45)"
}

// A first click can save the original string and still stop before the log
// verification passes. Then the queue must still let the user finish without
// pasting the string again, or the task would be a dead end.
// fixtureRunnableTask prepares a passed task whose baseline exists in a real
// workspace history, which is what manual evidence collection requires.
func fixtureRunnableTask(t *testing.T, root string) (string, client.WorkflowState) {
	t.Helper()
	path, state := fixturePassedTask(t, root)
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
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return path, state
}

func TestWorkbenchSessionReUsesSavedOriginalString(t *testing.T) {
	root := t.TempDir()
	path, state := fixtureRunnableTask(t, root)
	planPath := filepath.Join(filepath.Dir(path), "plan.json")
	plan := client.SubmissionPlan{ExecutionPrompt: "冻结题面", Payload: map[string]any{"repo_url": state.Project.RepoURL, "commit_ref": state.Project.BaselineCommit, "primary_language": state.Project.Language, "precheck_item_id": 99, "trae_session_id": savedSessionToken(), "effective_rounds": 12}}
	if err := client.SaveLocalJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	store := client.NewWorkflowStore(path)
	state, err := store.BindPlan(context.Background(), state.Revision, planPath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Trae.SessionTokenSHA256 == "" || state.Status == client.WorkflowSessionVerified {
		t.Fatalf("fixture status=%s session=%q", state.Status, state.Trae.SessionTokenSHA256)
	}
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.host = "127.0.0.1:8765"
	var commands []string
	w.runFlow = func(ctx context.Context, args ...string) ([]byte, error) {
		commands = append(commands, args[0])
		return []byte("{}"), nil
	}
	response := sessionPost(w, workbenchTaskKey(path), `{}`)
	if response.Code != 200 || len(commands) != 2 || commands[0] != "bind-plan" || commands[1] != "session-verify" {
		t.Fatalf("status=%d body=%s commands=%v", response.Code, response.Body.String(), commands)
	}

	// A task with no saved string cannot be verified from an empty request.
	emptyRoot := t.TempDir()
	emptyPath, _ := fixtureRunnableTask(t, emptyRoot)
	emptyServer, err := newWorkbenchServer(emptyRoot)
	if err != nil {
		t.Fatal(err)
	}
	emptyServer.host = "127.0.0.1:8765"
	emptyServer.runFlow = func(ctx context.Context, args ...string) ([]byte, error) { return []byte("{}"), nil }
	response = sessionPost(emptyServer, workbenchTaskKey(emptyPath), `{}`)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "尚无已保存的 Session 原串") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func traceTaskFixture(t *testing.T) (*workbenchServer, string, client.WorkflowState) {
	t.Helper()
	root := t.TempDir()
	path, state := fixturePassedTask(t, root)
	token := strings.Repeat("a", 16) + ":" + strings.Repeat("b", 32) + "_" + strings.Repeat("c", 24) + "." + strings.Repeat("d", 24) + "." + strings.Repeat("e", 24) + ":TraeCode CN.3.3.90.no_sid.no_ppe.T(2026/10/5 20:14:45)"
	plan := client.SubmissionPlan{ExecutionPrompt: "冻结题面", Payload: map[string]any{"repo_url": state.Project.RepoURL, "precheck_item_id": 99, "commit_ref": state.Project.BaselineCommit, "primary_language": "Go", "trae_session_id": token}}
	planPath := filepath.Join(filepath.Dir(path), "plan.json")
	if err := client.SaveLocalJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	store := client.NewWorkflowStore(path)
	state, err := store.BindPlan(context.Background(), state.Revision, planPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []client.WorkflowStatus{client.WorkflowTraeRunning, client.WorkflowSessionCaptured, client.WorkflowSessionVerified} {
		state, err = store.Update(context.Background(), state.Revision, client.WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *client.WorkflowState) error { s.Status = next; return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.host = "127.0.0.1:8765"
	w.runFlow = func(ctx context.Context, args ...string) ([]byte, error) {
		fresh, err := store.Load(ctx)
		if err != nil {
			return nil, err
		}
		_, err = store.BindPlan(ctx, fresh.Revision, fresh.PlanPath)
		return []byte(`{}`), err
	}
	toolsRoot, err := filepath.Abs("../../../tools")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SWE_FLOW_TOOLS", toolsRoot)
	if err := ensureTraceDirectory(path); err != nil {
		t.Fatal(err)
	}
	return w, path, state
}
func TestTraceDirectoryScanAndImportAreBoundToOneTask(t *testing.T) {
	w, path, state := traceTaskFixture(t)
	source := filepath.Join(traceDropDirectory(path), "本题轨迹.md")
	_ = os.WriteFile(source, []byte("**User**\n\n冻结题面\n\n**Assistant**\n\n真实格式测试轨迹\n"), 0600)
	_ = os.WriteFile(filepath.Join(traceDropDirectory(path), "notes.txt"), []byte("ignore"), 0600)
	_ = os.Symlink(source, filepath.Join(traceDropDirectory(path), "linked.md"))
	scan := httptest.NewRecorder()
	w.handler().ServeHTTP(scan, httptest.NewRequest("GET", "http://127.0.0.1:8765/api/tasks/"+workbenchTaskKey(path)+"/trace-files", nil))
	var found struct {
		Files []traceFileView `json:"files"`
	}
	_ = json.Unmarshal(scan.Body.Bytes(), &found)
	if scan.Code != 200 || len(found.Files) != 2 {
		t.Fatalf("scan %d %s", scan.Code, scan.Body.String())
	}
	hash := ""
	for _, f := range found.Files {
		if f.Name == "本题轨迹.md" {
			hash = f.SHA256
		}
		if f.Name == "linked.md" && f.Problem == "" {
			t.Fatal("symlink accepted")
		}
	}
	request := func(name, digest string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"name": name, "sha256": digest, "attested": true})
		req := httptest.NewRequest("POST", "http://127.0.0.1:8765/api/tasks/"+workbenchTaskKey(path)+"/trace-import", bytes.NewReader(body))
		req.Header.Set("Origin", "http://127.0.0.1:8765")
		req.Header.Set("X-CSRF-Token", w.csrf)
		out := httptest.NewRecorder()
		w.handler().ServeHTTP(out, req)
		return out
	}
	if response := request("../本题轨迹.md", hash); response.Code != 400 {
		t.Fatal("path traversal accepted")
	}
	if response := request("linked.md", hash); response.Code != 409 {
		t.Fatal("linked file accepted")
	}
	if response := request("本题轨迹.md", strings.Repeat("0", 64)); response.Code != 409 {
		t.Fatal("changed file accepted")
	}
	if response := request("本题轨迹.md", hash); response.Code != 200 {
		t.Fatalf("import %d %s", response.Code, response.Body.String())
	}
	fresh, err := client.NewWorkflowStore(path).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(fresh.Verification.Artifacts["x_trae_trace"].Path) != filepath.Join(traceDropDirectory(path), "archive") {
		t.Fatal("archive not under project trajectory folder")
	}
	if err := w.verifyTrace(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("drop file was removed")
	}
	if response := request("本题轨迹.md", hash); response.Code != 409 {
		t.Fatal("archive overwritten")
	}
	_ = state
}
func TestUploadedTraceUsesSameDirectoryAndFailedImportCanRetry(t *testing.T) {
	w, path, _ := traceTaskFixture(t)
	source := filepath.Join(traceDropDirectory(path), "bad.md")
	_ = os.WriteFile(source, []byte("not a native trace"), 0600)
	state, err := client.NewWorkflowStore(path).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.archiveTrace(context.Background(), path, state, source); err == nil {
		t.Fatal("bad trace archived")
	}
	if _, err := os.Stat(filepath.Join(traceDropDirectory(path), "archive")); !os.IsNotExist(err) {
		t.Fatal("failed import left an archive blocking retry")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "native.md")
	_, _ = fmt.Fprint(part, "**User**\n\n冻结题面\n\n**Assistant**\n\n模型响应\n")
	_ = writer.WriteField("attested", "true")
	_ = writer.Close()
	req := httptest.NewRequest("POST", "http://127.0.0.1:8765/api/tasks/"+workbenchTaskKey(path)+"/trace", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Origin", "http://127.0.0.1:8765")
	req.Header.Set("X-CSRF-Token", w.csrf)
	out := httptest.NewRecorder()
	w.handler().ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatalf("upload %d %s", out.Code, out.Body.String())
	}
	if _, err := os.Stat(filepath.Join(traceDropDirectory(path), "archive", "trae-export-original.private.md")); err != nil {
		t.Fatal(err)
	}
}

func TestTraceDirectoryBeforePlanUsesTaskFolder(t *testing.T) {
	state := client.WorkflowState{}
	got := traceArchiveDirectory(state, "/workflow/task/task-state.json")
	if got != "/workflow/task/trajectories/archive" {
		t.Fatalf("relative or unrelated archive directory: %s", got)
	}
}

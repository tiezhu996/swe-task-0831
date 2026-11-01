package main

import (
	"context"
	"encoding/json"
	"errors"
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

func gitFixture(t *testing.T, parent string) string {
	t.Helper()
	repo := filepath.Join(parent, "upstream")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("public source\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "README.md"}, {"-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "baseline"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	return repo
}

func TestWorkbenchWorkRootPersistsAndRejectsReviewDirectory(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "review")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.host = "127.0.0.1:8765"
	put := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"work_root": path})
		req := httptest.NewRequest("PUT", "http://127.0.0.1:8765/api/settings", strings.NewReader(string(body)))
		req.Header.Set("Origin", "http://127.0.0.1:8765")
		req.Header.Set("X-CSRF-Token", w.csrf)
		response := httptest.NewRecorder()
		w.handler().ServeHTTP(response, req)
		return response
	}
	if response := put(filepath.Join(root, "sources")); response.Code != 400 {
		t.Fatalf("review overlap accepted: %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "sources")); !os.IsNotExist(err) {
		t.Fatalf("rejected review directory was created: %v", err)
	}
	workRoot := filepath.Join(parent, "swe-work")
	if response := put(workRoot); response.Code != 200 {
		t.Fatalf("setting failed: %d %s", response.Code, response.Body.String())
	}
	if info, err := os.Stat(workRoot); err != nil || !info.IsDir() {
		t.Fatalf("work root not created: %v", err)
	}
	settings := filepath.Join(w.privateRoot, "settings.json")
	if info, err := os.Stat(settings); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private settings mode: %v", err)
	}
	restarted, err := newWorkbenchServer(root)
	resolvedRoot, _ := filepath.EvalSymlinks(workRoot)
	if err != nil || restarted.workRoot != resolvedRoot {
		t.Fatalf("setting not restored: root=%q err=%v", restarted.workRoot, err)
	}
}

func TestIsolatedPreparationKeepsReviewFilesOutOfModelSource(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "review")
	workRoot := filepath.Join(parent, "swe-work")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workRoot, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := gitFixture(t, parent)
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.cloneRepo = func(ctx context.Context, repoURL, target string) error {
		cmd := exec.CommandContext(ctx, "git", "clone", "-q", "--origin", "upstream", fixture, target)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("clone: %s: %w", output, err)
		}
		cmd = exec.CommandContext(ctx, "git", "-C", target, "remote", "set-url", "upstream", repoURL)
		return cmd.Run()
	}
	item := client.ItemDetail{BatchItem: client.BatchItem{ID: 1529, RepoURL: "https://github.com/example/repo", Status: "PRECHECK_PASSED"}, RequirementPrompt: "冻结题面只输入一次"}
	prepared, err := w.prepareIsolatedTask(context.Background(), item, "Go", workRoot)
	if err != nil {
		t.Fatal(err)
	}
	path := prepared.StatePath
	state, err := client.NewWorkflowStore(path).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, _ := filepath.EvalSymlinks(workRoot)
	name := privateTaskName(item.ID, "repo")
	if state.Workspace != filepath.Join(resolvedRoot, "sources", name) || filepath.Dir(path) != filepath.Join(resolvedRoot, "workflow", name) {
		t.Fatalf("source and workflow are not siblings: workspace=%q state=%q", state.Workspace, path)
	}
	if pathsOverlap(state.Workspace, filepath.Dir(path)) || !pathContains(resolvedRoot, state.Workspace) || pathContains(state.Workspace, w.privateRoot) {
		t.Fatalf("review data overlaps model workspace: workspace=%q private=%q state=%q", state.Workspace, filepath.Dir(path), w.privateRoot)
	}
	for _, name := range []string{"PROMPT.md", "TASK.md", "rubric.txt", "source-preparation.json", "task-state.json"} {
		if _, err := os.Stat(filepath.Join(state.Workspace, name)); !os.IsNotExist(err) {
			t.Fatalf("private material visible in model source: %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "PROMPT.md")); err != nil {
		t.Fatal(err)
	}
	var receipt sourcePreparation
	if err := client.ReadLocalJSON(filepath.Join(filepath.Dir(path), "source-preparation.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	head, err := gitOutput(context.Background(), state.Workspace, "rev-parse", "HEAD")
	if err != nil || receipt.BaselineCommit != strings.TrimSpace(string(head)) || receipt.PrecheckID != item.ID || receipt.PromptSHA256 != client.TextSHA256(item.RequirementPrompt) || len(receipt.TrackedFiles) != 1 {
		t.Fatalf("unverified baseline receipt: %+v err=%v", receipt, err)
	}
	if _, err := w.prepareIsolatedTask(context.Background(), item, "Go", workRoot); err == nil {
		t.Fatal("duplicate preparation overwrote an existing workspace")
	} else if !strings.Contains(err.Error(), "同编号任务已存在") {
		t.Fatalf("duplicate preparation failed for the wrong reason: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(resolvedRoot, stalePreparationArchive)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a live task was moved into the stale archive")
	}
	store := client.NewWorkflowStore(path)
	bound, err := store.Update(context.Background(), state.Revision, client.WorkflowAudit{Action: "fixture_precheck", Outcome: "success"}, func(s *client.WorkflowState) error {
		s.Precheck = client.WorkflowPrecheck{ItemID: item.ID, PromptSHA256: client.TextSHA256(item.RequirementPrompt)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPreparedSource(context.Background(), path, bound); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state.Workspace, "README.md"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyPreparedSource(context.Background(), path, bound); err == nil {
		t.Fatal("modified source passed pre-run check")
	}
}

func TestHistoricalPrecheckMarkerBlocksFreshStart(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "trdsql-task", "tasks", "task1")
	if err := os.MkdirAll(taskDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "TASK.md"), []byte("预检记录：`#2410`\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	if !w.hasExistingPrecheck(context.Background(), 2410) {
		t.Fatal("historical evidence was ignored")
	}
	if w.hasExistingPrecheck(context.Background(), 2411) {
		t.Fatal("unrelated precheck was blocked")
	}
}

func TestStartPrecheckCreatesOneIsolatedTask(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "review")
	workRoot := filepath.Join(parent, "swe-work")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workRoot, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := gitFixture(t, parent)
	platform := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		fmt.Fprint(out, `{"code":0,"data":{"id":1529,"batch_id":"batch-1","repo_url":"https://github.com/example/repo","requirement_prompt":"冻结题面只输入一次","status":"PRECHECK_PASSED"}}`)
	}))
	defer platform.Close()
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.host = "127.0.0.1:8765"
	w.workRoot, _ = filepath.EvalSymlinks(workRoot)
	w.newClient = func(context.Context) (*client.Client, error) { return client.New(platform.URL, nil) }
	w.resolveLanguage = func(context.Context, string) (string, error) { return "Go", nil }
	w.cloneRepo = func(ctx context.Context, repoURL, target string) error {
		if err := exec.CommandContext(ctx, "git", "clone", "-q", "--origin", "upstream", fixture, target).Run(); err != nil {
			return err
		}
		return exec.CommandContext(ctx, "git", "-C", target, "remote", "set-url", "upstream", repoURL).Run()
	}
	w.runFlow = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] != "precheck-bind" {
			return nil, fmt.Errorf("schema unavailable in fixture")
		}
		statePath := args[2]
		store := client.NewWorkflowStore(statePath)
		state, err := store.Load(ctx)
		if err != nil {
			return nil, err
		}
		for _, next := range []client.WorkflowStatus{client.WorkflowPrecheckDraft, client.WorkflowPrecheckSubmitted, client.WorkflowPrecheckPassed} {
			state, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "fixture_precheck_bind", Outcome: "success"}, func(s *client.WorkflowState) error {
				s.Precheck = client.WorkflowPrecheck{ItemID: 1529, BatchID: "batch-1", Status: "PRECHECK_PASSED", PromptSHA256: client.TextSHA256("冻结题面只输入一次")}
				s.Status = next
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		return []byte(`{}`), err
	}
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://127.0.0.1:8765/api/prechecks/1529/start", strings.NewReader(`{"confirm":true}`))
		req.Header.Set("Origin", "http://127.0.0.1:8765")
		req.Header.Set("X-CSRF-Token", w.csrf)
		response := httptest.NewRecorder()
		w.handler().ServeHTTP(response, req)
		return response
	}
	first := request()
	if first.Code != 201 || !strings.Contains(first.Body.String(), `"plan_ready":false`) {
		t.Fatalf("first start: %d %s", first.Code, first.Body.String())
	}
	handoffRequest := httptest.NewRequest("GET", "http://127.0.0.1:8765/api/prechecks/1529/handoff?mode=manual", nil)
	handoff := httptest.NewRecorder()
	w.handler().ServeHTTP(handoff, handoffRequest)
	var copied struct {
		Prompt    string `json:"prompt"`
		Mode      string `json:"execution_mode"`
		CodexText string `json:"codex_text"`
	}
	if handoff.Code != 200 || json.Unmarshal(handoff.Body.Bytes(), &copied) != nil || copied.Prompt != "冻结题面只输入一次" || copied.Mode != "manual" || !strings.Contains(copied.CodexText, "验收和提交") || !strings.Contains(copied.CodexText, w.workRoot) || !strings.Contains(copied.CodexText, "用户只负责补充 Session 和原生轨迹") || strings.Contains(copied.CodexText, "你需要在目标源码工作区单独操作 Trae") {
		t.Fatalf("handoff did not use frozen full prompt: %d %s", handoff.Code, handoff.Body.String())
	}
	second := request()
	if second.Code != 409 {
		t.Fatalf("duplicate start: %d %s", second.Code, second.Body.String())
	}
	paths, err := w.taskPaths()
	if err != nil || len(paths) != 1 {
		t.Fatalf("task paths=%v err=%v", paths, err)
	}
	state, err := client.NewWorkflowStore(paths[0]).Load(context.Background())
	if err != nil || state.Status != client.WorkflowPrecheckPassed || state.Precheck.ItemID != 1529 || state.Trae.ExecutionMode != "manual" || state.Trae.ExecutionStartedAt != "" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if filepath.Dir(paths[0]) != filepath.Join(w.workRoot, "workflow", privateTaskName(1529, "repo")) {
		t.Fatalf("state is outside sibling workflow directory: %q", paths[0])
	}
}

func TestGithubRepoPartsRejectsUntrustedHosts(t *testing.T) {
	for _, raw := range []string{"https://github.com.attacker.test/o/r", "https://github.com/o/r/extra", "https://github.com/o/r?token=x", "https://user@github.com/o/r"} {
		if _, _, err := githubRepoParts(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestCanonicalProjectNamePreservesRepositoryName(t *testing.T) {
	for _, repo := range []string{"ezbookkeeping", "Project_X.v2", "long-project-name-that-must-not-be-truncated-anymore"} {
		if got := privateTaskName(1528, repo); got != "1528-"+repo {
			t.Fatalf("name=%q", got)
		}
	}
	if got := workbenchProjectName(1528, "https://github.com/mayswind/ezbookkeeping", "precheck-1528-ezbookkeeping"); got != "1528-ezbookkeeping" {
		t.Fatalf("legacy display=%q", got)
	}
	if got := workbenchProjectName(0, "https://github.com/o/r", "unbound"); got != "unbound" {
		t.Fatal(got)
	}
}

func TestChangingWorkRootKeepsRecordedTasksVisible(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	root := filepath.Join(parent, "review")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(parent, "sources", "1529-repo")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	recordTask := func(workRoot, name, repo string, precheck int, status client.WorkflowStatus) string {
		t.Helper()
		dir := filepath.Join(workRoot, "workflow", name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		wsp := filepath.Join(parent, "sources", name)
		if err := os.MkdirAll(wsp, 0700); err != nil {
			t.Fatal(err)
		}
		store := client.NewWorkflowStore(filepath.Join(dir, "task-state.json"))
		state, err := store.Init(ctx, name, wsp)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *client.WorkflowState) error {
			s.Project = client.WorkflowProject{RepoURL: repo, BaselineCommit: strings.Repeat("a", 40), Language: "Go"}
			s.Precheck.ItemID = precheck
			s.Status = status
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Join(dir, "task-state.json")
	}
	recordTask(root, "1529-repo", "https://github.com/example/repo", 1529, client.WorkflowLocalOnly)

	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.host = "127.0.0.1:8765"
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	put := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"work_root": path})
		req := httptest.NewRequest("PUT", "http://127.0.0.1:8765/api/settings", strings.NewReader(string(body)))
		req.Header.Set("Origin", "http://127.0.0.1:8765")
		req.Header.Set("X-CSRF-Token", w.csrf)
		response := httptest.NewRecorder()
		w.handler().ServeHTTP(response, req)
		return response
	}
	tasks := func(w *workbenchServer) []workbenchTask {
		t.Helper()
		req := httptest.NewRequest("GET", "http://127.0.0.1:8765/api/tasks", nil)
		out := httptest.NewRecorder()
		w.handler().ServeHTTP(out, req)
		if out.Code != 200 {
			t.Fatalf("tasks: %d %s", out.Code, out.Body.String())
		}
		var body struct {
			Tasks []workbenchTask `json:"tasks"`
		}
		if err := json.Unmarshal(out.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Tasks
	}
	if listed := tasks(w); len(listed) != 1 || listed[0].PrecheckID != 1529 {
		t.Fatalf("baseline tasks=%+v", listed)
	}

	newRoot := filepath.Join(parent, "work-1021")
	if response := put(newRoot); response.Code != 200 {
		t.Fatalf("settings: %d %s", response.Code, response.Body.String())
	}
	resolvedNewRoot, err := filepath.EvalSymlinks(newRoot)
	if err != nil {
		t.Fatal(err)
	}
	listed := tasks(w)
	if len(listed) != 1 {
		t.Fatalf("changing the work root hid recorded tasks: %+v", listed)
	}
	if listed[0].PrecheckID != 1529 || listed[0].Status != string(client.WorkflowLocalOnly) || listed[0].Stage != "dropped" {
		t.Fatalf("recorded task lost its state: %+v", listed[0])
	}
	if !listed[0].Historical || listed[0].WorkRoot != resolvedRoot {
		t.Fatalf("recorded task should be marked historical in %q: %+v", resolvedRoot, listed[0])
	}

	recordTask(resolvedNewRoot, "1530-repo", "https://github.com/example/repo", 1530, client.WorkflowProjectSelected)
	listed = tasks(w)
	if len(listed) != 2 {
		t.Fatalf("new work root tasks=%+v", listed)
	}
	historical := 0
	for _, task := range listed {
		if task.Historical {
			historical++
			if task.WorkRoot != resolvedRoot {
				t.Fatalf("unexpected historical root: %+v", task)
			}
		} else if task.WorkRoot != resolvedNewRoot {
			t.Fatalf("new task should belong to the active root: %+v", task)
		}
	}
	if historical != 1 {
		t.Fatalf("historical count=%d tasks=%+v", historical, listed)
	}

	// The retired directory is persisted, so a restart still finds the task.
	restarted, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	restarted.host = "127.0.0.1:8765"
	if listed = tasks(restarted); len(listed) != 2 {
		t.Fatalf("restart lost tasks: %+v", listed)
	}
}

func TestValidateWorkRootKeepsCurrentReviewRootUsable(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "review")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(parent, "private")
	if _, err := validateWorkRoot(resolved, private, resolved, ""); err == nil {
		t.Fatal("a new selection may not point at the review root")
	}
	got, err := validateWorkRoot(resolved, private, resolved, resolved)
	if err != nil || got != resolved {
		t.Fatalf("already configured review root rejected: %q %v", got, err)
	}
	other := filepath.Join(parent, "work-1021")
	if _, err := validateWorkRoot(resolved, private, other, resolved); err != nil {
		t.Fatalf("separate directory rejected: %v", err)
	}
}

func TestStartLanguageUsesRecordedDesignInsteadOfGitHub(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	t.Setenv("SWE_PRECHECK_DESIGN_DIR", filepath.Join(parent, "design-registry"))
	prompt := "归档当前查询后保留固定版本，后续数据变化可比较。"
	research := filepath.Join(parent, "research.md")
	if err := os.WriteFile(research, []byte("固定基线查询代码研究"), 0600); err != nil {
		t.Fatal(err)
	}
	materials, schema := designRecordFixture(t, strings.Repeat("a", 40), prompt, research)
	registry, err := client.DefaultPrecheckDesignStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Save(ctx, prompt, "Python", materials, schema); err != nil {
		t.Fatal(err)
	}
	item := client.ItemDetail{BatchItem: client.BatchItem{ID: 5114, RepoURL: "https://github.com/example/repo", Status: "PRECHECK_PASSED"}, RequirementPrompt: prompt}
	if _, err := registry.Bind(ctx, item); err != nil {
		t.Fatal(err)
	}
	w := &workbenchServer{resolveLanguage: func(context.Context, string) (string, error) {
		t.Error("已记录设计不应再请求 GitHub")
		return "", fmt.Errorf("rate limited")
	}}
	language, err := w.startLanguage(ctx, item)
	if err != nil || language != "Python" {
		t.Fatalf("language=%q err=%v", language, err)
	}
}

func TestStartLanguageFallsBackToResolverWithoutDesign(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SWE_PRECHECK_DESIGN_DIR", filepath.Join(t.TempDir(), "design-registry"))
	item := client.ItemDetail{BatchItem: client.BatchItem{ID: 6001, RepoURL: "https://github.com/example/other", Status: "PRECHECK_PASSED"}, RequirementPrompt: "题面原文"}
	calls := 0
	w := &workbenchServer{resolveLanguage: func(context.Context, string) (string, error) {
		calls++
		return "Go", nil
	}}
	if language, err := w.startLanguage(ctx, item); err != nil || language != "Go" || calls != 1 {
		t.Fatalf("language=%q calls=%d err=%v", language, calls, err)
	}
	w.resolveLanguage = func(context.Context, string) (string, error) {
		return "", fmt.Errorf("仓库主要语言读取失败（GitHub 返回 HTTP 403）")
	}
	if _, err := w.startLanguage(ctx, item); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected GitHub failure to surface, got %v", err)
	}
}

func TestGithubPrimaryLanguageUsesTokenAndParsesLanguage(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "fixture-token")
	var authorization, path string
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		authorization, path = r.Header.Get("Authorization"), r.URL.Path
		fmt.Fprint(out, `{"language":"Python"}`)
	}))
	defer server.Close()
	previous := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	defer func() { githubAPIBaseURL = previous }()
	language, err := githubPrimaryLanguage(context.Background(), "https://github.com/GitGuardian/ggshield")
	if err != nil || language != "Python" || authorization != "Bearer fixture-token" || path != "/repos/GitGuardian/ggshield" {
		t.Fatalf("language=%q authorization=%q path=%q err=%v", language, authorization, path, err)
	}
}

func TestGithubPrimaryLanguageReportsHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		out.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	previous := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	defer func() { githubAPIBaseURL = previous }()
	if _, err := githubPrimaryLanguage(context.Background(), "https://github.com/GitGuardian/ggshield"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected HTTP 403 in error, got %v", err)
	}
}

// A preparation that died after creating the canonical directories must not
// force a manual folder check on every retry: the leftovers move into a
// timestamped archive and the same precheck can be prepared again.
func TestStalePreparationIsArchivedInsteadOfBlocking(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "review")
	workRoot := filepath.Join(parent, "swe-work")
	for _, dir := range []string{root, workRoot} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fixture := gitFixture(t, parent)
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.cloneRepo = func(ctx context.Context, repoURL, target string) error {
		if err := exec.CommandContext(ctx, "git", "clone", "-q", "--origin", "upstream", fixture, target).Run(); err != nil {
			return err
		}
		return exec.CommandContext(ctx, "git", "-C", target, "remote", "set-url", "upstream", repoURL).Run()
	}
	item := client.ItemDetail{BatchItem: client.BatchItem{ID: 5107, RepoURL: "https://github.com/example/repo", Status: "PRECHECK_PASSED"}, RequirementPrompt: "冻结题面只输入一次"}
	resolvedRoot, _ := filepath.EvalSymlinks(workRoot)
	w.mu.Lock()
	w.workRoot = resolvedRoot
	w.mu.Unlock()
	name := privateTaskName(item.ID, "repo")
	leftoverSource := filepath.Join(resolvedRoot, "sources", name)
	leftoverWorkflow := filepath.Join(resolvedRoot, "workflow", name)
	// An interrupted attempt: an empty source directory plus the workflow
	// directories it created before it gave up.
	if err := os.MkdirAll(leftoverSource, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(leftoverWorkflow, "trajectories"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftoverWorkflow, "PROMPT.md"), []byte("半截材料\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := w.prepareIsolatedTask(context.Background(), item, "Go", workRoot)
	if err != nil {
		t.Fatalf("leftover directories blocked a new preparation: %v", err)
	}
	if prepared.Archived == "" {
		t.Fatal("leftover directories were not archived")
	}
	if filepath.Dir(prepared.Archived) != filepath.Join(resolvedRoot, stalePreparationArchive) {
		t.Fatalf("archive is not inside the work root archive: %q", prepared.Archived)
	}
	kept, err := os.ReadFile(filepath.Join(prepared.Archived, "workflow", name, "PROMPT.md"))
	if err != nil || string(kept) != "半截材料\n" {
		t.Fatalf("archived attempt lost its material: %q err=%v", kept, err)
	}
	state, err := client.NewWorkflowStore(prepared.StatePath).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Workspace != leftoverSource {
		t.Fatalf("new preparation did not reuse the canonical source path: %q", state.Workspace)
	}
	// The archive never resurfaces as a live task, even when it holds a state
	// file: the leftover is history, not work in progress.
	if err := os.WriteFile(filepath.Join(prepared.Archived, "workflow", name, "task-state.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	paths, err := w.taskPaths()
	if err != nil || len(paths) != 1 || paths[0] != prepared.StatePath {
		t.Fatalf("archived attempt leaked into the task list: %v err=%v", paths, err)
	}
}

// A failed preparation must not leave the canonical directories behind: they
// would block the retry with a conflict created by the previous attempt.
func TestFailedPreparationLeavesNoResidue(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "review")
	workRoot := filepath.Join(parent, "swe-work")
	for _, dir := range []string{root, workRoot} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fixture := gitFixture(t, parent)
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.cloneRepo = func(context.Context, string, string) error { return fmt.Errorf("克隆公开仓库失败") }
	item := client.ItemDetail{BatchItem: client.BatchItem{ID: 5108, RepoURL: "https://github.com/example/repo", Status: "PRECHECK_PASSED"}, RequirementPrompt: "冻结题面只输入一次"}
	if _, err := w.prepareIsolatedTask(context.Background(), item, "Go", workRoot); err == nil {
		t.Fatal("failed clone passed")
	}
	resolvedRoot, _ := filepath.EvalSymlinks(workRoot)
	name := privateTaskName(item.ID, "repo")
	for _, dir := range []string{filepath.Join(resolvedRoot, "sources", name), filepath.Join(resolvedRoot, "workflow", name)} {
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed preparation left %s behind", dir)
		}
	}
	if _, err := os.Lstat(filepath.Join(resolvedRoot, stalePreparationArchive)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed preparation archived something")
	}
	w.cloneRepo = func(ctx context.Context, repoURL, target string) error {
		if err := exec.CommandContext(ctx, "git", "clone", "-q", "--origin", "upstream", fixture, target).Run(); err != nil {
			return err
		}
		return exec.CommandContext(ctx, "git", "-C", target, "remote", "set-url", "upstream", repoURL).Run()
	}
	if _, err := w.prepareIsolatedTask(context.Background(), item, "Go", workRoot); err != nil {
		t.Fatalf("retry after a failed preparation: %v", err)
	}
}

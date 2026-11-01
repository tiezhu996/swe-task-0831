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
)

func designRecordFixture(t *testing.T, baseline, prompt, source string) (client.DesignMaterials, client.FormSchema) {
	t.Helper()
	var text []string
	text = append(text, "rubrics:")
	for id := 1; id <= 5; id++ {
		kind := "f2p"
		if id == 5 {
			kind = "p2p"
		}
		text = append(text, fmt.Sprintf("- id: %d\n  type: %s\n  text: 给定输入 %d 后保存独立结果，后续修改不改变已保存记录。", id, kind, id))
	}
	evidence, err := client.HashWorkflowArtifact(source)
	if err != nil {
		t.Fatal(err)
	}
	material := client.DesignMaterials{SchemaVersion: 1, Origin: "before_execution", RepoURL: "https://github.com/example/repo", BaselineCommit: baseline, PromptSHA256: client.TextSHA256(prompt), Fields: map[string]string{"task_type": "功能新增", "authenticity_note": "原始源码只有实时查询，快照与差异比较涉及持久化与查询语义。", "involved_modules": "实时查询、快照持久化、差异比较", "verify_rubric": strings.Join(text, "\n")}, Basis: "按设计时基线研究查询边界", SourceEvidence: []client.WorkflowArtifact{evidence}}
	schema := client.FormSchema{Version: "v1", Fields: []client.FormField{{Key: "repo_url", Type: "url"}, {Key: "precheck_item_id", Type: "number"}, {Key: "commit_ref", Type: "text"}, {Key: "primary_language", Type: "text"}, {Key: "task_type", Type: "select", Options: []string{"功能新增"}}, {Key: "authenticity_note", Type: "textarea"}, {Key: "involved_modules", Type: "textarea"}, {Key: "verify_rubric", Type: "textarea"}}}
	return material, schema
}
func TestRecordedDesignControlsBaselineAndAutomaticallyFreezesTask(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	t.Setenv("SWE_PRECHECK_DESIGN_DIR", filepath.Join(parent, "design-registry"))
	repo := gitFixture(t, parent)
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	baseline := strings.TrimSpace(string(head))
	prompt := "归档当前查询后保留固定版本，后续数据变化可比较。"
	research := filepath.Join(parent, "research.md")
	_ = os.WriteFile(research, []byte("固定基线查询代码研究"), 0600)
	materials, schema := designRecordFixture(t, baseline, prompt, research)
	registry, err := client.DefaultPrecheckDesignStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Save(ctx, prompt, "Go", materials, schema); err != nil {
		t.Fatal(err)
	}
	// Default branch moves after research: preparation must still use its baseline.
	_ = os.WriteFile(filepath.Join(repo, "README.md"), []byte("later upstream change"), 0600)
	for _, args := range [][]string{{"add", "README.md"}, {"-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "new head"}} {
		if err := exec.Command("git", append([]string{"-C", repo}, args...)...).Run(); err != nil {
			t.Fatal(err)
		}
	}
	reviewRoot := filepath.Join(parent, "review")
	workRoot := filepath.Join(parent, "work")
	_ = os.Mkdir(reviewRoot, 0700)
	_ = os.Mkdir(workRoot, 0700)
	w, err := newWorkbenchServer(reviewRoot)
	if err != nil {
		t.Fatal(err)
	}
	w.cloneRepo = func(ctx context.Context, raw, target string) error {
		if err := exec.CommandContext(ctx, "git", "clone", "-q", "--origin", "upstream", repo, target).Run(); err != nil {
			return err
		}
		return exec.CommandContext(ctx, "git", "-C", target, "remote", "set-url", "upstream", raw).Run()
	}
	item := client.ItemDetail{BatchItem: client.BatchItem{ID: 6001, RepoURL: materials.RepoURL, Status: "PRECHECK_PASSED"}, RequirementPrompt: prompt}
	prepared, err := w.prepareIsolatedTask(ctx, item, "Go", workRoot)
	if err != nil {
		t.Fatal(err)
	}
	path := prepared.StatePath
	store := client.NewWorkflowStore(path)
	state, err := store.Load(ctx)
	if err != nil || state.Project.BaselineCommit != baseline {
		t.Fatalf("did not pin design baseline %s %v", state.Project.BaselineCommit, err)
	}
	for _, next := range []client.WorkflowStatus{client.WorkflowPrecheckDraft, client.WorkflowPrecheckSubmitted, client.WorkflowPrecheckPassed} {
		state, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *client.WorkflowState) error {
			s.Precheck = client.WorkflowPrecheck{ItemID: 6001, Status: "PRECHECK_PASSED", PromptSHA256: client.TextSHA256(prompt)}
			s.Status = next
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	schemaPath := filepath.Join(filepath.Dir(path), "schema.json")
	_ = client.SaveLocalJSON(schemaPath, schema)
	state, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *client.WorkflowState) error {
		s.Submission.SchemaPath = schemaPath
		s.Submission.SchemaSHA256 = client.FormSchemaSHA256(schema)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.runFlow = func(ctx context.Context, args ...string) ([]byte, error) {
		fresh, err := store.Load(ctx)
		if err != nil {
			return nil, err
		}
		switch args[0] {
		case "prepare-plan":
			plan, err := client.PrepareWorkflowPlan(fresh, client.SubmissionPlan{Payload: map[string]any{}}, schema, prompt)
			if err != nil {
				return nil, err
			}
			plan.SchemaSHA256 = client.FormSchemaSHA256(schema)
			return []byte(`{}`), client.SaveLocalJSON(filepath.Join(filepath.Dir(path), "submission-plan.json"), plan)
		case "bind-plan":
			_, err := store.BindPlan(ctx, fresh.Revision, filepath.Join(filepath.Dir(path), "submission-plan.json"))
			return []byte(`{}`), err
		}
		return nil, fmt.Errorf("unexpected flow")
	}
	if err := w.prepareTaskPlan(ctx, path, state); err != nil {
		t.Fatal(err)
	}
	state, err = store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := client.ReadSubmissionPlan(state.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyDesignBinding(state, plan); err != nil {
		t.Fatal(err)
	}
	for _, key := range client.DesignFieldKeys {
		if plan.Payload[key] != materials.Fields[key] {
			t.Fatalf("%s not reused", key)
		}
	}
	if err := w.reusePrecheckDesign(ctx, path); err != nil {
		t.Fatal("repeat adoption failed", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "design", "rubric.yaml")); err != nil {
		t.Fatal(err)
	}
}
func TestPrecheckPageReadsDesignAndRejectsRecordingForExecutedTask(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SWE_PRECHECK_DESIGN_DIR", filepath.Join(root, "registry"))
	path, state := fixturePassedTask(t, root)
	research := filepath.Join(root, "research.md")
	_ = os.WriteFile(research, []byte("原始源码研究"), 0600)
	materials, schema := designRecordFixture(t, state.Project.BaselineCommit, "冻结题面", research)
	registry, _ := client.DefaultPrecheckDesignStore()
	_, err := registry.Save(context.Background(), "冻结题面", "Go", materials, schema)
	if err != nil {
		t.Fatal(err)
	}
	platform := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		fmt.Fprint(out, `{"code":0,"data":{"id":99,"repo_url":"https://github.com/example/repo","requirement_prompt":"冻结题面","status":"PRECHECK_PASSED"}}`)
	}))
	defer platform.Close()
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.newClient = func(context.Context) (*client.Client, error) { return client.New(platform.URL, nil) }
	out := httptest.NewRecorder()
	w.handler().ServeHTTP(out, httptest.NewRequest("GET", "/api/prechecks/99/design", nil))
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"approved":true`) || !strings.Contains(out.Body.String(), "快照持久化") {
		t.Fatalf("design details %d %s", out.Code, out.Body.String())
	}
	store := client.NewWorkflowStore(path)
	_, err = store.Update(context.Background(), state.Revision, client.WorkflowAudit{Action: "fixture", Outcome: "success"}, func(s *client.WorkflowState) error { s.Status = client.WorkflowTraeRunning; return nil })
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "http://example.com/api/prechecks/99/design", strings.NewReader(`{"language":"Go","materials":{}}`))
	request.Header.Set("Origin", "http://example.com")
	request.Header.Set("X-CSRF-Token", w.csrf)
	out = httptest.NewRecorder()
	w.handler().ServeHTTP(out, request)
	if out.Code != 409 || !strings.Contains(out.Body.String(), "本题已执行") {
		t.Fatalf("post-run record status %d %s", out.Code, out.Body.String())
	}
}

func TestNewPrecheckRequiresPairedDesignAndApprovalBindsIt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("SWE_PRECHECK_DESIGN_DIR", filepath.Join(root, "registry"))
	path, state := fixturePassedTask(t, root)
	var schemaCalls int
	research := filepath.Join(root, "research.md")
	_ = os.WriteFile(research, []byte("按固定基线研究查询和保存行为"), 0600)
	materials, schema := designRecordFixture(t, state.Project.BaselineCommit, "冻结题面", research)
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("paired design attempted platform write")
		}
		switch r.URL.Path {
		case "/submissions/form-schema":
			schemaCalls++
			_ = json.NewEncoder(out).Encode(map[string]any{"code": 0, "data": schema})
		case "/precheck/items/99":
			fmt.Fprint(out, `{"code":0,"data":{"id":99,"batch_id":"one","repo_url":"https://github.com/example/repo","requirement_prompt":"冻结题面","status":"PRECHECK_PASSED"}}`)
		default:
			http.NotFound(out, r)
		}
	}))
	defer server.Close()
	platform, err := client.New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordDraftForPrecheck(ctx, state, "冻结题面", filepath.Dir(path), platform); err == nil || schemaCalls != 0 {
		t.Fatal("missing paired design was not blocked locally")
	}
	if err := client.SaveLocalJSON(filepath.Join(filepath.Dir(path), "design-draft.json"), materials); err != nil {
		t.Fatal(err)
	}
	if err := recordDraftForPrecheck(ctx, state, "冻结题面", filepath.Dir(path), platform); err != nil {
		t.Fatal(err)
	}
	registry, _ := client.DefaultPrecheckDesignStore()
	if _, err := registry.Find(ctx, state.Project.RepoURL, "冻结题面"); err != nil {
		t.Fatal("paired design was not persisted", err)
	}
	// Read-only status synchronization should bind the record to the true id.
	if err := syncPrecheck(ctx, platform, client.NewWorkflowStore(path), state); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Approved(ctx, 99, state.Project.RepoURL, state.Precheck.PromptSHA256); err != nil {
		t.Fatal("approval did not bind the design", err)
	}
}

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	client "example.com/swe-precheck-client"
)

//go:embed web/*
var workbenchFiles embed.FS

type workbenchServer struct {
	root                string
	privateRoot         string
	workRoot            string
	previousWorkRoots   []string
	csrf                string
	host                string
	mu                  sync.Mutex
	precheckMu          sync.Mutex
	prepareMu           sync.Mutex
	formMu              sync.Mutex
	evidenceMu          sync.Mutex
	fieldUnlocks        map[string]workbenchFieldUnlock
	prechecks           []client.BatchItem
	prechecksAt         time.Time
	precheckHashes      map[int]cachedPrecheckBinding
	prechecksRefreshing bool
	prechecksError      bool
	prechecksAttemptAt  time.Time
	// archivedPrechecks shelves platform prechecks that never became a local
	// task; their state lives here because the platform owns no such flag.
	archivedPrechecks []int
	// Proxy settings apply only to GitHub project pulls (clone/fetch and the
	// public language lookup), never to the local platform endpoint.
	proxyEnabled    bool
	proxyScheme     string
	proxyHost       string
	proxyPort       int
	newClient       func(context.Context) (*client.Client, error)
	runFlow         func(context.Context, ...string) ([]byte, error)
	cloneRepo       func(context.Context, string, string) error
	resolveLanguage func(context.Context, string) (string, error)
	// repoCache keeps one bare mirror per upstream repository so repeated
	// prechecks of the same project do not clone it again. A nil cache keeps
	// the historical direct clone, which is also what the tests rely on.
	repoCache  *repoMirrorCache
	runSubmit  func(context.Context, string, string) error
	submitJobs map[string]*workbenchSubmitJob
	gate       platformGate
}

type workbenchTask struct {
	Key                string              `json:"key"`
	TaskID             string              `json:"task_id"`
	DisplayName        string              `json:"display_name"`
	Path               string              `json:"path"`
	Status             string              `json:"status"`
	RepoURL            string              `json:"repo_url"`
	Baseline           string              `json:"baseline"`
	PrecheckID         int                 `json:"precheck_id"`
	SubmissionID       int                 `json:"submission_id"`
	Rounds             *int                `json:"rounds,omitempty"`
	ExecutionMode      string              `json:"execution_mode,omitempty"`
	ExecutionStartedAt string              `json:"execution_started_at,omitempty"`
	PromptSHA256       string              `json:"prompt_sha256"`
	PlanSHA256         string              `json:"plan_sha256"`
	SessionSHA256      string              `json:"session_sha256"`
	SessionSkipped     bool                `json:"session_verification_skipped,omitempty"`
	Artifacts          map[string]bool     `json:"artifacts"`
	NextAction         string              `json:"next_action"`
	IntegrityError     bool                `json:"integrity_error"`
	Pending            bool                `json:"pending"`
	PreflightPassed    bool                `json:"preflight_passed"`
	Workspace          string              `json:"workspace"`
	Revision           int64               `json:"revision"`
	NextStep           string              `json:"next_step"`
	Stage              string              `json:"stage"`
	ReviewReady        bool                `json:"review_ready"`
	ReviewReason       string              `json:"review_reason,omitempty"`
	SubmissionStatus   string              `json:"submission_status,omitempty"`
	QuestionType       string              `json:"question_type,omitempty"`
	MoreThanTwoHours   string              `json:"more_than_two_hours,omitempty"`
	WorkRoot           string              `json:"work_root,omitempty"`
	Historical         bool                `json:"historical,omitempty"`
	Archived           bool                `json:"archived,omitempty"`
	SubmitJob          *workbenchSubmitJob `json:"submit_job,omitempty"`
}

func runWorkbenchServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	root := flags.String("root", ".", "SWE task workspace root")
	port := flags.Int("port", 8765, "local HTTP port; 0 selects a free port")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *port < 0 || *port > 65535 {
		return fmt.Errorf("invalid local port")
	}
	absolute, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("workspace root is not a directory")
	}
	w, err := newWorkbenchServer(absolute)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	defer listener.Close()
	w.host = listener.Addr().String()
	cacheCtx, stopCache := context.WithCancel(context.Background())
	defer stopCache()
	w.startPrecheckCache(cacheCtx)
	fmt.Printf("http://%s/\n", w.host)
	return http.Serve(listener, w.handler())
}

func newWorkbenchServer(root string) (*workbenchServer, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	privateRoot, err := workbenchPrivateRoot(resolvedRoot)
	if err != nil {
		return nil, err
	}
	if info, err := os.Lstat(privateRoot); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("私有任务目录不能是符号链接")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(privateRoot, 0700); err != nil {
		return nil, fmt.Errorf("无法建立私有任务目录")
	}
	info, err := os.Stat(privateRoot)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("私有任务目录权限必须限制为 0700")
	}
	privateRoot, err = filepath.EvalSymlinks(privateRoot)
	if err != nil {
		return nil, err
	}
	if pathsOverlap(resolvedRoot, privateRoot) {
		return nil, fmt.Errorf("私有任务目录不能与工作台仓库重叠")
	}
	w := &workbenchServer{
		root:        resolvedRoot,
		privateRoot: privateRoot,
		csrf:        hex.EncodeToString(secret),
		newClient:   authenticatedClient,
		// Loon's default local HTTP proxy. The user can change it in Settings;
		// the proxy stays disabled until they turn it on.
		proxyScheme: "http",
		proxyHost:   "127.0.0.1",
		proxyPort:   7890,
	}
	if err := w.loadSettings(); err != nil {
		return nil, err
	}
	w.repoCache = newRepoMirrorCache(w.privateRoot)
	w.loadPrecheckCache()
	w.runFlow = func(ctx context.Context, args ...string) ([]byte, error) {
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, executable, args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("workflow step failed: %s", compactFlowError(output))
		}
		return output, nil
	}
	w.runSubmit = runWorkbenchSubmitProcess
	w.cloneRepo = func(ctx context.Context, repoURL, target string) error {
		return clonePublicRepository(ctx, repoURL, target, w.gitProxyURL())
	}
	w.resolveLanguage = func(ctx context.Context, repoURL string) (string, error) {
		return githubPrimaryLanguageWithProxy(ctx, repoURL, w.gitProxyURL())
	}
	return w, nil
}

func compactFlowError(output []byte) string {
	message := strings.TrimSpace(string(output))
	if len(message) > 300 {
		message = message[:300]
	}
	if message == "" {
		return "inspect task state and local evidence"
	}
	return message
}

func (w *workbenchServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/bootstrap", w.bootstrap)
	mux.HandleFunc("GET /api/settings", w.getSettings)
	mux.HandleFunc("PUT /api/settings", w.setSettings)
	mux.HandleFunc("POST /api/settings/test-network", w.testNetwork)
	mux.HandleFunc("GET /api/swe/accounts", w.listSWEAccounts)
	mux.HandleFunc("POST /api/swe/accounts", w.addSWEAccount)
	mux.HandleFunc("POST /api/swe/accounts/switch", w.switchSWEAccount)
	mux.HandleFunc("POST /api/swe/accounts/remove", w.removeSWEAccount)
	mux.HandleFunc("GET /api/tasks", w.listTasks)
	mux.HandleFunc("POST /api/tasks", w.createTask)
	mux.HandleFunc("GET /api/tasks/{key}", w.taskDetail)
	mux.HandleFunc("GET /api/tasks/{key}/execution", w.executionDetails)
	mux.HandleFunc("GET /api/tasks/{key}/form", w.getForm)
	mux.HandleFunc("PUT /api/tasks/{key}/form", w.putForm)
	mux.HandleFunc("POST /api/tasks/{key}/form/unlock", w.unlockFormField)
	mux.HandleFunc("POST /api/tasks/{key}/prepare", w.preparePlan)
	mux.HandleFunc("POST /api/tasks/{key}/precheck", w.submitPrecheck)
	mux.HandleFunc("POST /api/tasks/{key}/precheck-refresh", w.refreshTaskPrecheck)
	mux.HandleFunc("POST /api/tasks/{key}/trae-start", w.traeStart)
	mux.HandleFunc("POST /api/tasks/{key}/session", w.captureSession)
	mux.HandleFunc("POST /api/tasks/{key}/trace", w.uploadTrace)
	mux.HandleFunc("GET /api/tasks/{key}/trace-files", w.traceFiles)
	mux.HandleFunc("POST /api/tasks/{key}/trace-import", w.importTraceFile)
	mux.HandleFunc("GET /api/tasks/{key}/design", w.designDetails)
	mux.HandleFunc("GET /api/tasks/{key}/receipts", w.receipts)
	mux.HandleFunc("GET /api/tasks/{key}/journal", w.journal)
	mux.HandleFunc("POST /api/tasks/{key}/audit", w.auditTask)
	mux.HandleFunc("POST /api/tasks/{key}/authorize", w.authorizeTask)
	mux.HandleFunc("GET /api/tasks/{key}/handoff", w.handoff)
	mux.HandleFunc("GET /api/tasks/{key}/submit", w.submitStatus)
	mux.HandleFunc("POST /api/tasks/{key}/submit", w.submitTask)
	mux.HandleFunc("POST /api/tasks/{key}/local-only", w.setLocalOnly)
	mux.HandleFunc("POST /api/tasks/{key}/archive", w.setTaskArchived)
	mux.HandleFunc("POST /api/tasks/{key}/submission-refresh", w.refreshSubmission)
	mux.HandleFunc("GET /api/platform", w.platformStatus)
	mux.HandleFunc("GET /api/prechecks", w.listPrechecks)
	mux.HandleFunc("POST /api/prechecks/{id}/archive", w.setPrecheckArchived)
	mux.HandleFunc("GET /api/prechecks/{id}/handoff", w.precheckHandoff)
	mux.HandleFunc("GET /api/prechecks/{id}/design", w.precheckDesignDetails)
	mux.HandleFunc("POST /api/prechecks/{id}/design", w.savePrecheckDesign)
	mux.HandleFunc("POST /api/prechecks/{id}/import", w.importPrecheck)
	mux.HandleFunc("POST /api/prechecks/{id}/start", w.startPrecheck)
	assets, _ := fs.Sub(workbenchFiles, "web")
	mux.Handle("/", noStoreAssets(http.FileServer(http.FS(assets))))
	return http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.Host != w.host && w.host != "" {
			http.Error(out, "invalid host", http.StatusForbidden)
			return
		}
		out.Header().Set("Cache-Control", "no-store")
		out.Header().Set("X-Content-Type-Options", "nosniff")
		out.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("X-CSRF-Token") != w.csrf {
				http.Error(out, "invalid local request", http.StatusForbidden)
				return
			}
		}
		mux.ServeHTTP(out, r)
	})
}

// The web assets are embedded in the binary and change on every rebuild.
// Never let a web view reuse an old app.js/style.css across launches.
func noStoreAssets(next http.Handler) http.Handler {
	return http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		out.Header().Set("Cache-Control", "no-store, max-age=0")
		next.ServeHTTP(out, r)
	})
}

func writeWorkbenchJSON(out http.ResponseWriter, code int, value any) {
	out.Header().Set("Content-Type", "application/json; charset=utf-8")
	out.WriteHeader(code)
	_ = json.NewEncoder(out).Encode(value)
}

func workbenchError(out http.ResponseWriter, code int, message string) {
	writeWorkbenchJSON(out, code, map[string]string{"error": message})
}

func decodeWorkbenchJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("extra request data")
	}
	return nil
}

func (w *workbenchServer) bootstrap(out http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	workRoot := w.workRoot
	w.mu.Unlock()
	writeWorkbenchJSON(out, http.StatusOK, map[string]string{"csrf": w.csrf, "root": w.root, "work_root": workRoot})
}

func workbenchTaskKey(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:12])
}

func (w *workbenchServer) taskPaths() ([]string, error) {
	var paths []string
	seen := map[string]bool{}
	add := func(path string) {
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	walk := func(base string) error {
		return filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				if path != base {
					name := entry.Name()
					if name == ".git" || name == "node_modules" || name == ".playwright-cli" || name == "sources" || name == "verification" || name == "research" || name == "precheck-client" || name == "tools" || name == stalePreparationArchive {
						return filepath.SkipDir
					}
					if rel, _ := filepath.Rel(base, path); strings.Count(rel, string(filepath.Separator)) >= 8 {
						return filepath.SkipDir
					}
				}
				return nil
			}
			if entry.Name() == "task-state.json" {
				add(path)
			}
			return nil
		})
	}
	w.mu.Lock()
	workRoot := w.workRoot
	previousRoots := append([]string(nil), w.previousWorkRoots...)
	w.mu.Unlock()
	if workRoot == "" {
		if err := walk(w.root); err != nil {
			return nil, err
		}
		if _, err := os.Stat(w.privateRoot); err == nil {
			if err := walk(w.privateRoot); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	} else {
		// The active directory decides where new tasks are written. Retired
		// directories and the review root's own workflow directory keep their
		// recorded tasks visible, so changing the global directory cannot turn
		// finished work back into an unprocessed precheck.
		for index, root := range append([]string{workRoot}, previousRoots...) {
			if err := collectWorkflowTaskPaths(root, add); err != nil {
				if index == 0 {
					return nil, err
				}
				// A missing or broken archival directory must not blank the
				// whole queue.
				continue
			}
		}
		_ = collectWorkflowTaskPaths(w.root, add)
		return paths, nil
	}
	return paths, nil
}

// taskWorkRoot returns <root> for a canonical <root>/workflow/<task>/
// task-state.json path and "" for legacy task directories.
func taskWorkRoot(path string) string {
	workflow := filepath.Dir(filepath.Dir(path))
	if filepath.Base(workflow) != "workflow" {
		return ""
	}
	return filepath.Dir(workflow)
}

// collectWorkflowTaskPaths adds every canonical task below
// <root>/workflow/<task>/task-state.json. The scan stays shallow on purpose:
// older workbench versions also scattered hand-made task-state files through
// the repository, and re-importing those would resurrect abandoned runs.
func collectWorkflowTaskPaths(workRoot string, add func(string)) error {
	workflowRoot := filepath.Join(workRoot, "workflow")
	info, err := os.Lstat(workflowRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("工作流目录不是普通目录")
	}
	entries, err := os.ReadDir(workflowRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(workflowRoot, entry.Name(), "task-state.json")
		if info, err := os.Lstat(path); err == nil {
			if info.Mode().IsRegular() {
				add(path)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (w *workbenchServer) loadTask(ctx context.Context, key string) (string, client.WorkflowState, error) {
	paths, err := w.taskPaths()
	if err != nil {
		return "", client.WorkflowState{}, err
	}
	for _, path := range paths {
		if workbenchTaskKey(path) == key {
			state, err := client.NewWorkflowStore(path).Load(ctx)
			return path, state, err
		}
	}
	return "", client.WorkflowState{}, os.ErrNotExist
}

func (w *workbenchServer) taskView(ctx context.Context, path string) workbenchTask {
	view := workbenchTask{Key: workbenchTaskKey(path), Path: path, Artifacts: map[string]bool{}}
	if root := taskWorkRoot(path); root != "" {
		w.mu.Lock()
		current := w.workRoot
		w.mu.Unlock()
		view.WorkRoot = root
		view.Historical = current != "" && root != current
	}
	state, err := client.NewWorkflowStore(path).Load(ctx)
	if err != nil {
		view.IntegrityError = true
		planPath := ""
		data, readErr := os.ReadFile(path)
		if readErr == nil {
			var snapshot client.WorkflowState
			if json.Unmarshal(data, &snapshot) == nil {
				view.TaskID, view.Status = snapshot.TaskID, string(snapshot.Status)
				view.RepoURL, view.PrecheckID, view.SubmissionID = snapshot.Project.RepoURL, snapshot.Precheck.ItemID, snapshot.Submission.ID
				view.Archived = snapshot.Archived
				planPath = snapshot.PlanPath
			}
		}
		view.NextAction = "核查任务审计链"
		view.DisplayName = workbenchProjectName(view.PrecheckID, view.RepoURL, view.TaskID)
		view.Stage = "blocked"
		view.QuestionType, view.MoreThanTwoHours = planSampleFields(planPath)
		return view
	}
	// A finished run the collection policy will not take is closed here, on the
	// first view: the platform would reject it, so the queue must stop asking for
	// the remaining steps. It stays editable through the reopen action.
	if client.LocalOnlyEligible(state.Status) {
		if plan, planErr := client.ReadSubmissionPlan(state.PlanPath); planErr == nil {
			if blocked, _ := client.LocalOnlyDecision(state, plan); blocked {
				store := client.NewWorkflowStore(path)
				if advanced, advanceErr := store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "local_only", Outcome: "success"}, func(s *client.WorkflowState) error {
					s.Status = client.WorkflowLocalOnly
					return nil
				}); advanceErr == nil {
					state = advanced
				}
			}
		}
	}
	view.TaskID, view.Status = state.TaskID, string(state.Status)
	view.Workspace = state.Workspace
	view.RepoURL, view.Baseline = state.Project.RepoURL, state.Project.BaselineCommit
	view.PrecheckID, view.SubmissionID = state.Precheck.ItemID, state.Submission.ID
	view.DisplayName = workbenchProjectName(view.PrecheckID, view.RepoURL, view.TaskID)
	view.Rounds, view.PromptSHA256, view.PlanSHA256 = state.Trae.EffectiveRounds, state.Precheck.PromptSHA256, state.PlanSHA256
	view.ExecutionMode = state.Trae.ExecutionMode
	if view.ExecutionMode == "" && (state.Status == client.WorkflowPrecheckPassed || state.Status == client.WorkflowTraeRunning) {
		view.ExecutionMode = "manual"
	}
	view.Revision = state.Revision
	view.ExecutionStartedAt = state.Trae.ExecutionStartedAt
	view.SessionSHA256 = state.Trae.SessionTokenSHA256
	view.SessionSkipped = state.Trae.SessionVerificationSkipped
	for key := range state.Verification.Artifacts {
		view.Artifacts[key] = true
	}
	view.NextAction = actionForState(state, map[string]string{}).Action
	view.NextStep = actionForState(state, map[string]string{}).Reason
	view.Pending = state.PendingOperation != nil && (state.PendingOperation.Outcome == "started" || state.PendingOperation.Outcome == "uncertain")
	view.PreflightPassed = state.Verification.Preflight.Passed
	view.SubmissionStatus = state.Submission.Status
	gate := evaluateReviewGate(path, state)
	view.ReviewReady, view.ReviewReason = gate.Ready, gate.Reason
	view.SubmitJob = w.submitJob(view.Key, path)
	view.Stage = workbenchStage(view, state, gate, view.SubmitJob)
	view.Archived = state.Archived
	view.QuestionType, view.MoreThanTwoHours = planSampleFields(state.PlanPath)
	return view
}

// planSampleFields exposes the two recorded submission fields the queue shows
// beside difficulty and rounds: the sample type (question_type) and whether the
// run lasted more than two hours (x_is_more_than_two_hours). Both are read
// verbatim from the task's submission plan; a missing or unreadable plan leaves
// them empty, and they are never guessed from local timing or round counts.
func planSampleFields(planPath string) (questionType, moreThanTwoHours string) {
	if planPath == "" {
		return "", ""
	}
	plan, err := client.ReadSubmissionPlan(planPath)
	if err != nil {
		return "", ""
	}
	return planText(plan.Payload["question_type"]), planText(plan.Payload["x_is_more_than_two_hours"])
}

func planText(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func (w *workbenchServer) listTasks(out http.ResponseWriter, r *http.Request) {
	paths, err := w.taskPaths()
	if err != nil {
		workbenchError(out, 500, "无法读取任务目录")
		return
	}
	items := make([]workbenchTask, 0, len(paths))
	for _, path := range paths {
		w.reconcileDetectedTrae(r.Context(), path)
		items = append(items, w.taskView(r.Context(), path))
	}
	writeWorkbenchJSON(out, 200, map[string]any{"tasks": items})
}

func (w *workbenchServer) taskDetail(out http.ResponseWriter, r *http.Request) {
	path, _, err := w.loadTask(r.Context(), r.PathValue("key"))
	if err != nil {
		workbenchError(out, 409, "任务审计链不可用或任务不存在")
		return
	}
	w.reconcileDetectedTrae(r.Context(), path)
	writeWorkbenchJSON(out, 200, w.taskView(r.Context(), path))
}

func (w *workbenchServer) safeRelativePath(relative string, mustExist bool) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) == "." || strings.HasPrefix(filepath.Clean(relative), "..") {
		return "", fmt.Errorf("path must be relative to workspace root")
	}
	target := filepath.Join(w.root, relative)
	rel, err := filepath.Rel(w.root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace root")
	}
	check := target
	if !mustExist {
		check = filepath.Dir(target)
	}
	resolved, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", err
	}
	rel, err = filepath.Rel(w.root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path resolves outside workspace root")
	}
	if mustExist {
		return resolved, nil
	}
	return filepath.Join(resolved, filepath.Base(target)), nil
}

type newTaskRequest struct {
	TaskDir   string `json:"task_dir"`
	Workspace string `json:"workspace"`
	TaskID    string `json:"task_id"`
	RepoURL   string `json:"repo_url"`
	Baseline  string `json:"baseline"`
	Language  string `json:"language"`
	Prompt    string `json:"prompt"`
}

func (w *workbenchServer) createTaskFiles(ctx context.Context, input newTaskRequest, frozen bool) (string, error) {
	if filepath.Clean(filepath.Dir(input.TaskDir)) == "tasks" {
		parent := filepath.Join(w.root, "tasks")
		if err := os.Mkdir(parent, 0700); err != nil && !os.IsExist(err) {
			return "", fmt.Errorf("无法建立任务目录")
		}
	}
	taskDir, err := w.safeRelativePath(input.TaskDir, false)
	if err != nil {
		return "", err
	}
	workspace, err := w.safeRelativePath(input.Workspace, true)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		return "", fmt.Errorf("源码工作区不存在")
	}
	if taskDir == workspace || strings.HasPrefix(taskDir, workspace+string(filepath.Separator)) || strings.HasPrefix(workspace, taskDir+string(filepath.Separator)) {
		return "", fmt.Errorf("任务材料目录不能与模型源码工作区重叠")
	}
	if !strings.HasPrefix(input.RepoURL, "https://github.com/") || input.Baseline == "" || input.Language == "" || input.TaskID == "" {
		return "", fmt.Errorf("缺少原仓库、基线、语言或任务标识")
	}
	prompt := client.CanonicalPrompt(input.Prompt)
	if frozen {
		// A remote frozen prompt is bound verbatim; its length is the platform's
		// business, not the local 240-rune drafting rule's.
		if prompt != input.Prompt || prompt == "" {
			return "", fmt.Errorf("远端冻结题面无法逐字绑定")
		}
	} else if err := client.ValidateDraftPrompt(prompt); err != nil {
		return "", err
	}
	git := exec.CommandContext(ctx, "git", "-C", workspace, "cat-file", "-e", input.Baseline+"^{commit}")
	if git.Run() != nil {
		return "", fmt.Errorf("基线 commit 不在源码工作区")
	}
	head, err := exec.CommandContext(ctx, "git", "-C", workspace, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != input.Baseline {
		return "", fmt.Errorf("源码工作区 HEAD 必须等于固定基线")
	}
	changed, err := exec.CommandContext(ctx, "git", "-C", workspace, "status", "--porcelain").Output()
	if err != nil || len(changed) != 0 {
		return "", fmt.Errorf("源码工作区必须保持干净，旧改动需另存诊断")
	}
	if _, err := os.Lstat(taskDir); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("任务目录已存在或不可检查")
	}
	if err := os.Mkdir(taskDir, 0700); err != nil {
		return "", err
	}
	if err := os.Mkdir(filepath.Join(taskDir, "trajectories"), 0700); err != nil {
		return "", err
	}
	promptPath := filepath.Join(taskDir, "PROMPT.md")
	if err := os.WriteFile(promptPath, []byte(prompt+"\n"), 0600); err != nil {
		return "", err
	}
	statePath := filepath.Join(taskDir, "task-state.json")
	store := client.NewWorkflowStore(statePath)
	state, err := store.Init(ctx, input.TaskID, workspace)
	if err != nil {
		return "", err
	}
	_, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "project_bind", Outcome: "success"}, func(s *client.WorkflowState) error {
		s.Project = client.WorkflowProject{RepoURL: input.RepoURL, BaselineCommit: input.Baseline, Language: input.Language}
		s.Trae.ExecutionMode = "manual"
		s.Status = client.WorkflowProjectSelected
		return nil
	})
	if err != nil {
		return "", err
	}
	return statePath, nil
}

func (w *workbenchServer) createTask(out http.ResponseWriter, r *http.Request) {
	var input newTaskRequest
	if decodeWorkbenchJSON(r, &input) != nil {
		workbenchError(out, 400, "任务参数格式不正确")
		return
	}
	path, err := w.createTaskFiles(r.Context(), input, false)
	if err != nil {
		workbenchError(out, 409, err.Error())
		return
	}
	writeWorkbenchJSON(out, 201, w.taskView(r.Context(), path))
}

func (w *workbenchServer) listPrechecks(out http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	cacheMissing := w.prechecksAt.IsZero()
	w.mu.Unlock()
	if r.URL.Query().Get("refresh") == "1" || cacheMissing {
		if err := w.ensurePrecheckCache(r.Context(), r.URL.Query().Get("refresh") == "1"); err != nil && cacheMissing {
			workbenchError(out, 503, "个人预检暂不可用，请稍后刷新")
			return
		}
	} else {
		w.refreshPrecheckCacheIfDue()
	}
	w.mu.Lock()
	prechecks := append([]client.BatchItem(nil), w.prechecks...)
	prechecksAt := w.prechecksAt
	hashes := make(map[int]cachedPrecheckBinding, len(w.precheckHashes))
	for id, binding := range w.precheckHashes {
		hashes[id] = binding
	}
	refreshing, syncError := w.prechecksRefreshing, w.prechecksError
	archived := append([]int(nil), w.archivedPrechecks...)
	w.mu.Unlock()
	paths, err := w.taskPaths()
	if err != nil {
		workbenchError(out, 500, "本地任务读取失败")
		return
	}
	views := make([]workbenchTask, 0, len(paths))
	for _, path := range paths {
		views = append(views, w.taskView(r.Context(), path))
	}
	type remoteView struct {
		client.BatchItem
		Matches                 []string         `json:"matches"`
		MatchUnverified         bool             `json:"match_unverified"`
		UnverifiedLocalCount    int              `json:"unverified_local_count"`
		HistoricalEvidenceCount int              `json:"historical_evidence_count"`
		LocalMatches            []map[string]any `json:"local_matches"`
		DesignRecorded          bool             `json:"design_recorded"`
		Archived                bool             `json:"archived,omitempty"`
	}
	items := make([]remoteView, 0, len(prechecks))
	archivedSet := make(map[int]bool, len(archived))
	for _, id := range archived {
		archivedSet[id] = true
	}
	historical, err := w.historicalPrecheckIDs()
	if err != nil {
		workbenchError(out, 500, "历史任务材料读取失败，预检列表暂不可核查")
		return
	}
	for _, item := range prechecks {
		view := remoteView{BatchItem: item, Matches: []string{}, LocalMatches: []map[string]any{}, HistoricalEvidenceCount: historical[item.ID], Archived: archivedSet[item.ID]}
		if binding, ok := hashes[item.ID]; ok {
			if registry, err := client.DefaultPrecheckDesignStore(); err == nil {
				if _, err := registry.Approved(r.Context(), item.ID, item.RepoURL, binding.PromptSHA256, false); err == nil {
					view.DesignRecorded = true
				}
			}
		}
		candidate := false
		for _, local := range views {
			if local.IntegrityError && local.PrecheckID == item.ID {
				view.UnverifiedLocalCount++
			}
			if !local.IntegrityError && local.RepoURL == item.RepoURL && (local.PrecheckID == 0 || local.PrecheckID == item.ID) {
				candidate = true
			}
		}
		if candidate {
			binding, verified := hashes[item.ID]
			if verified && binding.RepoURL == item.RepoURL {
				for _, local := range views {
					if !local.IntegrityError && local.RepoURL == item.RepoURL && local.PromptSHA256 == binding.PromptSHA256 && (local.PrecheckID == 0 || local.PrecheckID == item.ID) {
						view.Matches = append(view.Matches, local.Key)
						view.LocalMatches = append(view.LocalMatches, map[string]any{
							"key": local.Key, "task_id": local.TaskID, "display_name": local.DisplayName, "status": local.Status,
							"submission_id": local.SubmissionID, "rounds": local.Rounds,
						})
					}
				}
			} else {
				view.MatchUnverified = true
			}
		}
		// Hide consumed historical prechecks that have no canonical local
		// task. They remain in platform history and can still be queried
		// there, but must not pollute the active workbench queue.
		if len(view.Matches) == 0 && item.Status != "" && item.Status != "PRECHECK_PASSED" && item.Status != "PRECHECK_SUBMITTED" && item.Status != "PRECHECK_REJECTED" {
			continue
		}
		items = append(items, view)
	}
	writeWorkbenchJSON(out, 200, map[string]any{"items": items, "synced_at": prechecksAt.Format(time.RFC3339), "total": len(items), "cached": true, "refreshing": refreshing, "sync_error": syncError, "stale": time.Since(prechecksAt) >= precheckCacheInterval, "refresh_interval_seconds": int(precheckCacheInterval.Seconds())})
}

func (w *workbenchServer) importPrecheck(out http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		workbenchError(out, 400, "预检编号无效")
		return
	}
	var input newTaskRequest
	if decodeWorkbenchJSON(r, &input) != nil {
		workbenchError(out, 400, "接入参数格式不正确")
		return
	}
	platform, err := w.newClient(r.Context())
	if err != nil {
		workbenchError(out, 503, "平台登录不可用")
		return
	}
	item, err := platform.ItemDetail(r.Context(), id)
	if err != nil || item.ID != id {
		workbenchError(out, 503, "预检详情读取失败")
		return
	}
	if item.Status != "PRECHECK_PASSED" {
		workbenchError(out, 409, "该预检当前不可作为新任务接入")
		return
	}
	if input.RepoURL != "" && input.RepoURL != item.RepoURL {
		workbenchError(out, 409, "仓库与预检不一致")
		return
	}
	input.RepoURL, input.Prompt = item.RepoURL, item.RequirementPrompt
	_, repo, repoErr := githubRepoParts(item.RepoURL)
	if repoErr != nil {
		workbenchError(out, 409, repoErr.Error())
		return
	}
	input.TaskID = privateTaskName(id, repo)
	if filepath.Base(input.TaskDir) != input.TaskID {
		workbenchError(out, 400, "任务目录名称应为 "+input.TaskID)
		return
	}
	path, err := w.createTaskFiles(r.Context(), input, true)
	if err != nil {
		workbenchError(out, 409, err.Error())
		return
	}
	_, err = w.runFlow(r.Context(), "precheck-bind", "-state", path, "-item-id", strconv.Itoa(id), "-prompt", filepath.Join(filepath.Dir(path), "PROMPT.md"))
	if err != nil {
		workbenchError(out, 409, "本地任务已建立，但预检绑定失败；请核查任务目录")
		return
	}
	writeWorkbenchJSON(out, 201, w.taskView(r.Context(), path))
}

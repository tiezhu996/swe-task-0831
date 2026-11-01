package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	client "example.com/swe-precheck-client"
)

type workbenchSettings struct {
	SchemaVersion int    `json:"schema_version"`
	WorkRoot      string `json:"work_root"`
	SourceRoot    string `json:"source_root,omitempty"` // Accept old settings without treating a source-only path as the work root.
	// PreviousWorkRoots keeps every directory the user has selected before.
	// The global work directory only decides where new tasks are written; a
	// directory change must never hide tasks that were recorded earlier.
	PreviousWorkRoots []string `json:"previous_work_roots,omitempty"`
	// ArchivedPrechecks shelves platform prechecks that have no local task.
	// Order is insertion order so a restore keeps the object it had.
	ArchivedPrechecks []int `json:"archived_prechecks,omitempty"`
	// Proxy settings for GitHub project pulls. Defaults are Loon's local HTTP
	// proxy; proxy_enabled starts false so a stopped proxy never breaks a pull.
	ProxyEnabled bool   `json:"proxy_enabled,omitempty"`
	ProxyScheme  string `json:"proxy_scheme,omitempty"`
	ProxyHost    string `json:"proxy_host,omitempty"`
	ProxyPort    int    `json:"proxy_port,omitempty"`
}

// maxWorkRootHistory bounds how many retired directories stay in the task
// scan. Eight is far beyond normal use and keeps startup cheap.
const maxWorkRootHistory = 8

// prepareTimeout bounds one source preparation. The first pull of a large
// repository routinely needs more than a few minutes; later pulls are served
// from the local mirror, so this generous deadline is paid at most once per
// repository.
const prepareTimeout = 60 * time.Minute

type sourcePreparation struct {
	SchemaVersion      int      `json:"schema_version"`
	PrecheckID         int      `json:"precheck_id"`
	RepoURL            string   `json:"repo_url"`
	BaselineCommit     string   `json:"baseline_commit"`
	PromptSHA256       string   `json:"prompt_sha256"`
	Workspace          string   `json:"workspace"`
	TrackedFiles       []string `json:"tracked_files"`
	TrackedFilesSHA256 string   `json:"tracked_files_sha256"`
	PreparedAt         string   `json:"prepared_at"`
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func pathsOverlap(a, b string) bool { return pathContains(a, b) || pathContains(b, a) }

func workbenchPrivateRoot(root string) (string, error) {
	base := os.Getenv("SWE_WORKBENCH_STATE_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state", "swe-task", "workbench")
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("私有任务目录必须为绝对路径")
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(base, hex.EncodeToString(sum[:8])), nil
}

func (w *workbenchServer) loadSettings() error {
	path := filepath.Join(w.privateRoot, "settings.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("工作台目录设置不可安全读取")
	}
	var settings workbenchSettings
	if err := client.ReadLocalJSON(path, &settings); err != nil || settings.SchemaVersion != 1 {
		return fmt.Errorf("工作台目录设置格式无效")
	}
	if settings.WorkRoot != "" {
		if !filepath.IsAbs(settings.WorkRoot) {
			return fmt.Errorf("全局工作目录设置格式无效")
		}
		w.workRoot = settings.WorkRoot
	}
	// A retired directory is historical context, not a setting: skip entries
	// that are malformed or duplicate the active directory instead of
	// refusing to start the whole workbench.
	for _, root := range settings.PreviousWorkRoots {
		if root == "" || !filepath.IsAbs(root) || root == w.workRoot || containsString(w.previousWorkRoots, root) {
			continue
		}
		w.previousWorkRoots = append(w.previousWorkRoots, root)
	}
	// Shelving a precheck is presentation state, so a malformed entry is
	// dropped instead of blocking the workbench from starting.
	for _, id := range settings.ArchivedPrechecks {
		if id > 0 && !containsInt(w.archivedPrechecks, id) {
			w.archivedPrechecks = append(w.archivedPrechecks, id)
		}
	}
	if settings.ProxyScheme != "" || settings.ProxyHost != "" || settings.ProxyPort != 0 {
		if scheme, host, port, proxyErr := normalizeProxyTarget(settings.ProxyScheme, settings.ProxyHost, settings.ProxyPort); proxyErr == nil {
			w.proxyScheme, w.proxyHost, w.proxyPort = scheme, host, port
			w.proxyEnabled = settings.ProxyEnabled
		}
	}
	return nil
}

func containsInt(values []int, expected int) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

// saveSettingsLocked persists every workbench setting. The caller holds w.mu,
// which is also what makes the archived-precheck list race free.
func (w *workbenchServer) saveSettingsLocked() error {
	if err := os.MkdirAll(w.privateRoot, 0700); err != nil {
		return err
	}
	return client.SaveLocalJSON(filepath.Join(w.privateRoot, "settings.json"), workbenchSettings{
		SchemaVersion:     1,
		WorkRoot:          w.workRoot,
		PreviousWorkRoots: w.previousWorkRoots,
		ArchivedPrechecks: w.archivedPrechecks,
		ProxyEnabled:      w.proxyEnabled,
		ProxyScheme:       w.proxyScheme,
		ProxyHost:         w.proxyHost,
		ProxyPort:         w.proxyPort,
	})
}

// rememberWorkRoots builds the directory history to persist when the global
// work directory changes. The active directory comes first, then the
// directories that still exist, newest first.
func (w *workbenchServer) rememberWorkRoots(next string) []string {
	roots := make([]string, 0, len(w.previousWorkRoots)+1)
	appendRoot := func(root string) {
		if root == "" || root == next || containsString(roots, root) {
			return
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			return
		}
		roots = append(roots, root)
	}
	appendRoot(w.workRoot)
	for _, root := range w.previousWorkRoots {
		appendRoot(root)
	}
	if len(roots) > maxWorkRootHistory {
		roots = roots[:maxWorkRootHistory]
	}
	return roots
}

func (w *workbenchServer) setSettings(out http.ResponseWriter, r *http.Request) {
	var input struct {
		WorkRoot     *string `json:"work_root"`
		ProxyEnabled *bool   `json:"proxy_enabled"`
		ProxyScheme  *string `json:"proxy_scheme"`
		ProxyHost    *string `json:"proxy_host"`
		ProxyPort    *int    `json:"proxy_port"`
	}
	if decodeWorkbenchJSON(r, &input) != nil {
		workbenchError(out, 400, "设置格式不正确")
		return
	}
	w.mu.Lock()
	currentRoot := w.workRoot
	scheme, host, port := w.proxyScheme, w.proxyHost, w.proxyPort
	enabled := w.proxyEnabled
	w.mu.Unlock()
	if input.ProxyScheme != nil {
		scheme = *input.ProxyScheme
	}
	if input.ProxyHost != nil {
		host = *input.ProxyHost
	}
	if input.ProxyPort != nil {
		port = *input.ProxyPort
	}
	scheme, host, port, err := normalizeProxyTarget(scheme, host, port)
	if err != nil {
		workbenchError(out, 400, err.Error())
		return
	}
	if input.ProxyEnabled != nil {
		enabled = *input.ProxyEnabled
	}
	nextRoot := currentRoot
	if input.WorkRoot != nil {
		nextRoot, err = validateWorkRoot(w.root, w.privateRoot, *input.WorkRoot, currentRoot)
		if err != nil {
			workbenchError(out, 400, err.Error())
			return
		}
	}
	w.mu.Lock()
	history := w.rememberWorkRoots(nextRoot)
	previousRoot, previousHistory := w.workRoot, w.previousWorkRoots
	previousEnabled, previousScheme, previousHost, previousPort := w.proxyEnabled, w.proxyScheme, w.proxyHost, w.proxyPort
	w.workRoot, w.previousWorkRoots = nextRoot, history
	w.proxyEnabled, w.proxyScheme, w.proxyHost, w.proxyPort = enabled, scheme, host, port
	if err := w.saveSettingsLocked(); err != nil {
		w.workRoot, w.previousWorkRoots = previousRoot, previousHistory
		w.proxyEnabled, w.proxyScheme, w.proxyHost, w.proxyPort = previousEnabled, previousScheme, previousHost, previousPort
		w.mu.Unlock()
		workbenchError(out, 500, "设置保存失败")
		return
	}
	view := map[string]any{
		"work_root":     w.workRoot,
		"proxy_enabled": w.proxyEnabled,
		"proxy_scheme":  w.proxyScheme,
		"proxy_host":    w.proxyHost,
		"proxy_port":    w.proxyPort,
		"proxy_url":     w.proxyURLFromFieldsLocked(),
	}
	w.mu.Unlock()
	writeWorkbenchJSON(out, 200, view)
}

// validateWorkRoot checks a directory selection. current is the directory that
// is already configured: older installations pointed it at the review root, and
// re-validating that exact directory must keep working instead of blocking the
// tasks that already live there. A different directory still has to be separate
// from both the review repository and the private configuration.
func validateWorkRoot(root, privateRoot, selected, current string) (string, error) {
	if !filepath.IsAbs(selected) || selected == "/" {
		return "", fmt.Errorf("请输入全局工作目录的绝对路径")
	}
	currentResolved := filepath.Clean(current)
	if current != "" {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			currentResolved = resolved
		}
	}
	reviewOverlap := func(resolved string) bool {
		return pathsOverlap(root, resolved) && resolved != currentResolved
	}
	selected = filepath.Clean(selected)
	info, err := os.Lstat(selected)
	if errors.Is(err, os.ErrNotExist) {
		parent := filepath.Dir(selected)
		parentResolved, parentErr := filepath.EvalSymlinks(parent)
		if parentErr != nil {
			return "", fmt.Errorf("工作目录的父目录不存在")
		}
		resolved := filepath.Join(parentResolved, filepath.Base(selected))
		if reviewOverlap(resolved) || pathsOverlap(privateRoot, resolved) {
			return "", fmt.Errorf("工作目录必须与评审仓库和私有配置分开")
		}
		if err := os.Mkdir(selected, 0700); err != nil {
			return "", fmt.Errorf("无法创建工作目录")
		}
	} else if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("工作目录必须是普通目录，不能是符号链接")
	}
	resolved, err := filepath.EvalSymlinks(selected)
	if err != nil {
		return "", fmt.Errorf("工作目录不可访问")
	}
	if reviewOverlap(resolved) || pathsOverlap(privateRoot, resolved) {
		return "", fmt.Errorf("工作目录必须与评审仓库和私有配置分开")
	}
	return resolved, nil
}

func (w *workbenchServer) precheckHandoff(out http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		workbenchError(out, 400, "预检编号无效")
		return
	}
	platform, err := w.newClient(r.Context())
	if err != nil {
		workbenchError(out, 503, "平台登录不可用")
		return
	}
	item, err := platform.ItemDetail(r.Context(), id)
	if err != nil || item.ID != id || item.RepoURL == "" || item.RequirementPrompt == "" || client.CanonicalPrompt(item.RequirementPrompt) != item.RequirementPrompt {
		workbenchError(out, 503, "预检原文读取失败，不能复制交接")
		return
	}
	w.mu.Lock()
	workRoot := w.workRoot
	w.mu.Unlock()
	manual := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("mode")), "manual")
	message := fmt.Sprintf("请处理 SWE 个人预检 #%d。原仓库：%s。先核对现有任务和历史执行能否复用；材料完整时沿用，不重复运行。冻结题面及其 SHA-256 如下，Trae 中只输入题面原文一次，不输入本段交接说明。\n\n%s\n\n题面 SHA-256：%s\n", id, item.RepoURL, item.RequirementPrompt, client.TextSHA256(item.RequirementPrompt))
	if workRoot != "" {
		message += fmt.Sprintf("全局工作目录：%s。新执行的源码放在 sources/<预检编号-仓库名>/，状态和验收材料放在平级 workflow/<预检编号-仓库名>/。Trae 只打开具体源码目录，不打开全局根目录。\n", workRoot)
	} else {
		message += "如需新执行，请先在工作台设置全局工作目录。\n"
	}
	if manual {
		paths, err := w.taskPaths()
		if err != nil {
			workbenchError(out, 409, "本地任务读取失败，不能生成交接")
			return
		}
		var matches []string
		for _, path := range paths {
			state, err := client.NewWorkflowStore(path).Load(r.Context())
			if err == nil && state.Precheck.ItemID == id && state.Project.RepoURL == item.RepoURL && state.Precheck.PromptSHA256 == client.TextSHA256(item.RequirementPrompt) {
				matches = append(matches, path)
			}
		}
		if len(matches) == 1 {
			state, err := client.NewWorkflowStore(matches[0]).Load(r.Context())
			if err != nil {
				workbenchError(out, 409, "任务状态变化，请刷新后复制")
				return
			}
			message = manualCodexHandoff(matches[0], state)
		} else if len(matches) > 1 {
			message += "发现多份本地执行，请先打开对应的本地任务，从该任务复制验收提交提示词，不能混用工作区或 Session。"
		} else {
			message += "当前尚未准备本地源码。手动执行前请先在工作台准备源码工作区；用户在 Trae 执行冻结题面后，只补充 Session 和原生 Markdown，再从本地任务复制验收提交提示词。不要把本段交接说明输入 Trae。"
		}
	} else {
		message += "请自行核对预检与仓库当前基线是否适配，推进可自动完成的本地核验；仅在 Trae 真实界面选模型、复制完整 Session 原串、原生导出 Markdown 等确需人工操作时提醒我。不要让模型读取 workflow、Rubric、旧答案或验收结论。审核时复核轨迹唯一人工题面、目标 Session 日志、源码差异和逐项验收，dry-run 通过后写入 workbench-review.json 即停止；正式提交由我在工作台点击执行。"
	}
	mode := "unspecified"
	if manual {
		mode = "manual"
	}
	writeWorkbenchJSON(out, 200, map[string]any{"prompt": item.RequirementPrompt, "prompt_sha256": client.TextSHA256(item.RequirementPrompt), "execution_mode": mode, "codex_text": message})
}

func githubRepoParts(raw string) (string, string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", fmt.Errorf("预检仓库不是可核验的 GitHub 地址")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("预检仓库路径不完整")
	}
	owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
	valid := regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	if !valid.MatchString(owner) || !valid.MatchString(repo) || owner == "." || repo == "." {
		return "", "", fmt.Errorf("预检仓库路径不合法")
	}
	return owner, repo, nil
}

// githubAPIBaseURL is a variable so tests can point the lookup at a local server.
var githubAPIBaseURL = "https://api.github.com"

func githubAPIToken() string {
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token
		}
	}
	return ""
}

func githubPrimaryLanguage(ctx context.Context, raw string) (string, error) {
	return githubPrimaryLanguageWithProxy(ctx, raw, "")
}

func githubPrimaryLanguageWithProxy(ctx context.Context, raw, proxyURL string) (string, error) {
	owner, repo, err := githubRepoParts(raw)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBaseURL+"/repos/"+owner+"/"+repo, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	if token := githubAPIToken(); token != "" {
		// Unauthenticated calls share a 60-request hourly quota per exit IP, so
		// an available token keeps this fallback lookup usable.
		request.Header.Set("Authorization", "Bearer "+token)
	}
	httpClient, clientErr := httpClientForProxy(proxyURL, 15*time.Second)
	if clientErr != nil {
		return "", fmt.Errorf("代理配置不可用: %w", clientErr)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("无法读取仓库主要语言")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return "", fmt.Errorf("仓库主要语言读取失败（GitHub 返回 HTTP %d）", response.StatusCode)
	}
	var metadata struct {
		Language string `json:"language"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&metadata) != nil || strings.TrimSpace(metadata.Language) == "" {
		return "", fmt.Errorf("仓库缺少可核验的主要语言")
	}
	return metadata.Language, nil
}

func clonePublicRepository(ctx context.Context, repoURL, target, proxyURL string) error {
	if _, _, err := githubRepoParts(repoURL); err != nil {
		return err
	}
	env := append(gitProxyEnv(os.Environ(), proxyURL), "GIT_TERMINAL_PROMPT=0")
	if err := gitRun(ctx, env, "clone", "--quiet", "--origin", "upstream", "--", repoURL, target); err != nil {
		return cloneFailureMessage(ctx, proxyURL, err)
	}
	return nil
}

// cloneFailureMessage reports what actually failed. A killed pull used to be
// reported as a proxy or repository problem even when the real cause was the
// deadline, which sent the user to check Loon for no reason.
func cloneFailureMessage(ctx context.Context, proxyURL string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("克隆超时：仓库较大或网络较慢，已下载内容会留在项目缓存中，重试可继续")
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("克隆已取消")
	}
	via := "直连"
	if proxyURL != "" {
		via = "代理 " + proxyURL
	}
	if detail := gitDetail(err); detail != "" {
		return fmt.Errorf("克隆公开仓库失败（%s）：%s", via, detail)
	}
	return fmt.Errorf("克隆公开仓库失败（%s），请检查网络或仓库是否公开", via)
}

// cloneProjectSource prepares the checked-out source for a new task. The local
// mirror is preferred; a cache that cannot be built falls back to the direct
// pull so a cache problem never blocks work the old path could finish.
func (w *workbenchServer) cloneProjectSource(ctx context.Context, repoURL, target string) error {
	w.mu.Lock()
	workRoot := w.workRoot
	w.mu.Unlock()
	if w.repoCache == nil || workRoot == "" {
		return w.cloneRepo(ctx, repoURL, target)
	}
	cacheErr := w.repoCache.clone(ctx, workRoot, repoURL, target, w.gitProxyURL())
	if cacheErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return cacheErr
	}
	if err := os.RemoveAll(target); err != nil {
		return cacheErr
	}
	if err := os.Mkdir(target, 0700); err != nil {
		return cacheErr
	}
	if err := w.cloneRepo(ctx, repoURL, target); err != nil {
		return fmt.Errorf("%v；直接克隆也失败：%s", cacheErr, gitDetail(err))
	}
	return nil
}

func gitOutput(ctx context.Context, workspace string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", workspace}, args...)...)
	return command.Output()
}

func inspectFreshSource(ctx context.Context, workspace, repoURL string) (string, []string, string, error) {
	head, err := gitOutput(ctx, workspace, "rev-parse", "HEAD")
	if err != nil || len(strings.TrimSpace(string(head))) != 40 {
		return "", nil, "", fmt.Errorf("源码基线不是完整 commit")
	}
	status, err := gitOutput(ctx, workspace, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || len(status) != 0 {
		return "", nil, "", fmt.Errorf("新源码工作区不干净")
	}
	remote, err := gitOutput(ctx, workspace, "remote", "get-url", "upstream")
	if err != nil || strings.TrimSpace(string(remote)) != repoURL {
		return "", nil, "", fmt.Errorf("源码仓库来源与预检不一致")
	}
	tracked, err := gitOutput(ctx, workspace, "ls-files", "-z")
	if err != nil || len(tracked) == 0 {
		return "", nil, "", fmt.Errorf("源码目录没有可核验文件")
	}
	parts := strings.Split(strings.TrimSuffix(string(tracked), "\x00"), "\x00")
	files := make([]string, 0, len(parts))
	for _, file := range parts {
		if file == "" || filepath.IsAbs(file) || strings.HasPrefix(filepath.Clean(file), "..") {
			return "", nil, "", fmt.Errorf("源码清单含非法路径")
		}
		files = append(files, file)
	}
	sum := sha256.Sum256(tracked)
	return strings.TrimSpace(string(head)), files, hex.EncodeToString(sum[:]), nil
}

func privateTaskName(id int, repo string) string {
	return fmt.Sprintf("%d-%s", id, strings.TrimSuffix(repo, ".git"))
}

func workbenchProjectName(id int, repoURL, fallback string) string {
	_, repo, err := githubRepoParts(repoURL)
	if id > 0 && err == nil {
		return privateTaskName(id, repo)
	}
	return fallback
}

var historicalPrecheckMarker = regexp.MustCompile(`(?:#|预检(?:记录)?\s*[:：]?\s*#?)([0-9]{2,8})`)

func (w *workbenchServer) historicalPrecheckIDs() (map[int]int, error) {
	counts := map[int]int{}
	err := filepath.WalkDir(w.root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != w.root {
				switch entry.Name() {
				case ".git", ".swe-workbench", ".swe-archive", "sources", "verification", "research", "node_modules", "precheck-client", "tools", ".playwright-cli":
					return filepath.SkipDir
				}
			}
			return nil
		}
		switch entry.Name() {
		case "TASK.md", "RUN_TASKS.md", "SUBMISSION.md":
		default:
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > 1<<20 {
			return fmt.Errorf("历史任务记录过大，无法核查")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		seen := map[int]bool{}
		for _, match := range historicalPrecheckMarker.FindAllSubmatch(data, -1) {
			id, _ := strconv.Atoi(string(match[1]))
			if id > 0 && !seen[id] {
				counts[id]++
				seen[id] = true
			}
		}
		return nil
	})
	return counts, err
}

func (w *workbenchServer) hasExistingPrecheck(ctx context.Context, id int) bool {
	paths, err := w.taskPaths()
	if err != nil {
		return true
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return true
		}
		var snapshot struct {
			Precheck struct {
				ItemID int `json:"item_id"`
			} `json:"precheck"`
		}
		if json.Unmarshal(data, &snapshot) != nil {
			return true
		}
		if snapshot.Precheck.ItemID == id {
			return true
		}
	}
	historical, err := w.historicalPrecheckIDs()
	return err != nil || historical[id] > 0
}

// preparedTask reports one finished source preparation. Archived is set when an
// abandoned earlier attempt had to be moved aside before the canonical
// directories could be created again.
type preparedTask struct {
	StatePath string
	Archived  string
}

// stalePreparationArchive holds the directories of abandoned preparation
// attempts. It is hidden and skipped by every task scan, so an archived attempt
// never reappears as a live task.
const stalePreparationArchive = ".swe-archive"

// archiveStalePreparation frees the canonical directory names again when an
// earlier attempt stopped after creating them. Nothing is deleted: the leftover
// directories move into a timestamped archive, so the same precheck can be
// prepared again without a manual folder check. A leftover that still holds
// task state is a real task and is never moved; that case keeps failing so the
// existing execution gets verified instead of silently duplicated.
func (w *workbenchServer) archiveStalePreparation(workRoot, name, source, workflow string) (string, error) {
	var stale []string
	for _, target := range []string{source, workflow} {
		info, err := os.Lstat(target)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return "", fmt.Errorf("同编号目录无法核查")
		case !info.IsDir() && info.Mode()&os.ModeSymlink == 0:
			return "", fmt.Errorf("同编号路径不是目录，请核查")
		}
		stale = append(stale, target)
	}
	if len(stale) == 0 {
		return "", nil
	}
	if _, err := os.Lstat(filepath.Join(workflow, "task-state.json")); err == nil {
		return "", fmt.Errorf("同编号任务已存在，请先核对现有执行")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("同编号任务状态无法核查")
	}
	archiveRoot := filepath.Join(workRoot, stalePreparationArchive)
	if err := os.Mkdir(archiveRoot, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("无法建立旧目录归档")
	}
	stamp := time.Now().Format("20060102-150405")
	destination := filepath.Join(archiveRoot, name+"-"+stamp)
	for suffix := 2; ; suffix++ {
		if _, err := os.Lstat(destination); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return "", fmt.Errorf("无法建立旧目录归档")
		}
		destination = filepath.Join(archiveRoot, fmt.Sprintf("%s-%s-%d", name, stamp, suffix))
	}
	for _, target := range stale {
		// Mirror the work root layout so an archived attempt can be moved back
		// by hand when it turns out to be worth keeping.
		held := filepath.Join(destination, filepath.Base(filepath.Dir(target)))
		if err := os.MkdirAll(held, 0700); err != nil {
			return "", fmt.Errorf("无法建立旧目录归档")
		}
		if err := os.Rename(target, filepath.Join(held, filepath.Base(target))); err != nil {
			return "", fmt.Errorf("旧目录无法移入归档 %s", destination)
		}
	}
	return destination, nil
}

func (w *workbenchServer) prepareIsolatedTask(ctx context.Context, item client.ItemDetail, language, workRoot string) (preparedTask, error) {
	resolvedRoot, err := filepath.EvalSymlinks(workRoot)
	if err != nil {
		return preparedTask{}, fmt.Errorf("全局工作目录不可访问")
	}
	workRoot = resolvedRoot
	_, repo, err := githubRepoParts(item.RepoURL)
	if err != nil {
		return preparedTask{}, err
	}
	name := privateTaskName(item.ID, repo)
	sourceParent := filepath.Join(workRoot, "sources")
	workflowParent := filepath.Join(workRoot, "workflow")
	for _, parent := range []string{sourceParent, workflowParent} {
		if info, err := os.Lstat(parent); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return preparedTask{}, fmt.Errorf("工作目录子目录不是普通目录")
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(parent, 0700); err != nil {
				return preparedTask{}, fmt.Errorf("无法创建工作目录子目录")
			}
		} else {
			return preparedTask{}, err
		}
	}
	source := filepath.Join(sourceParent, name)
	workflow := filepath.Join(workflowParent, name)
	archived, err := w.archiveStalePreparation(workRoot, name, source, workflow)
	if err != nil {
		return preparedTask{}, err
	}
	if err := os.Mkdir(source, 0700); err != nil {
		return preparedTask{}, fmt.Errorf("无法创建源码子目录")
	}
	if err := os.Mkdir(workflow, 0700); err != nil {
		_ = os.Remove(source)
		return preparedTask{}, fmt.Errorf("无法创建工作流子目录")
	}
	// A preparation that stops anywhere below must not leave the canonical
	// directories behind: they would block every later attempt with a folder
	// check nobody asked for.
	complete := false
	defer func() {
		if complete {
			return
		}
		_ = os.RemoveAll(workflow)
		_ = os.RemoveAll(source)
	}()
	if err := os.Mkdir(filepath.Join(workflow, "trajectories"), 0700); err != nil {
		return preparedTask{}, err
	}
	if err := w.cloneProjectSource(ctx, item.RepoURL, source); err != nil {
		return preparedTask{}, err
	}
	design, designErr := approvedPrecheckDesign(ctx, item)
	if designErr != nil && !errors.Is(designErr, os.ErrNotExist) {
		return preparedTask{}, fmt.Errorf("预检配套设计无法核对: %w", designErr)
	}
	if designErr == nil {
		if design.Language != language {
			return preparedTask{}, fmt.Errorf("仓库语言与已记录设计不同，请核查")
		}
		commit := design.Materials.BaselineCommit
		if exec.CommandContext(ctx, "git", "-C", source, "cat-file", "-e", commit+"^{commit}").Run() != nil {
			if err := w.gitFetch(ctx, source, "upstream", commit); err != nil {
				return preparedTask{}, fmt.Errorf("设计时的固定基线无法获取")
			}
		}
		if exec.CommandContext(ctx, "git", "-C", source, "checkout", "--detach", commit).Run() != nil {
			return preparedTask{}, fmt.Errorf("无法检出设计时的固定基线")
		}
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return preparedTask{}, fmt.Errorf("新源码目录不可核验")
	}
	baseline, files, filesHash, err := inspectFreshSource(ctx, source, item.RepoURL)
	if err != nil {
		return preparedTask{}, err
	}
	prompt := item.RequirementPrompt
	if prompt == "" || client.CanonicalPrompt(prompt) != prompt {
		return preparedTask{}, fmt.Errorf("预检题面不是可冻结的原文")
	}
	if err := os.WriteFile(filepath.Join(workflow, "PROMPT.md"), []byte(prompt+"\n"), 0600); err != nil {
		return preparedTask{}, err
	}
	receipt := sourcePreparation{SchemaVersion: 1, PrecheckID: item.ID, RepoURL: item.RepoURL, BaselineCommit: baseline, PromptSHA256: client.TextSHA256(prompt), Workspace: source, TrackedFiles: files, TrackedFilesSHA256: filesHash, PreparedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := client.SaveLocalJSON(filepath.Join(workflow, "source-preparation.json"), receipt); err != nil {
		return preparedTask{}, err
	}
	statePath := filepath.Join(workflow, "task-state.json")
	store := client.NewWorkflowStore(statePath)
	state, err := store.Init(ctx, name, source)
	if err != nil {
		return preparedTask{}, err
	}
	_, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "project_bind", Outcome: "success"}, func(s *client.WorkflowState) error {
		s.Project = client.WorkflowProject{RepoURL: item.RepoURL, BaselineCommit: baseline, Language: language}
		s.Trae.ExecutionMode = "manual"
		s.Status = client.WorkflowProjectSelected
		return nil
	})
	if err != nil {
		return preparedTask{}, err
	}
	complete = true
	return preparedTask{StatePath: statePath, Archived: archived}, nil
}

// startLanguage decides the language recorded for a new task. An approved local
// design already stores the language that was verified against the fixed
// baseline when the task was designed, so that record is reused instead of
// re-querying GitHub for the same precheck. The GitHub lookup stays as the
// fallback for prechecks without a design record; its unauthenticated quota is
// shared per exit IP and must not block a task that is otherwise fully verified
// locally.
func (w *workbenchServer) startLanguage(ctx context.Context, item client.ItemDetail) (string, error) {
	design, err := approvedPrecheckDesign(ctx, item)
	if err == nil {
		language := strings.TrimSpace(design.Language)
		if language == "" {
			return "", fmt.Errorf("已记录的设计缺少主要语言")
		}
		return language, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("预检配套设计无法核对: %w", err)
	}
	return w.resolveLanguage(ctx, item.RepoURL)
}

func (w *workbenchServer) startPrecheck(out http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		workbenchError(out, 400, "预检编号无效")
		return
	}
	var input struct {
		Confirm bool `json:"confirm"`
	}
	if decodeWorkbenchJSON(r, &input) != nil || !input.Confirm {
		workbenchError(out, 400, "需要确认本次准备")
		return
	}
	// A first pull of a large repository can hold the preparation slot for a
	// while; a second click must say so instead of queueing behind it.
	if !w.prepareMu.TryLock() {
		workbenchError(out, 409, "另一个源码工作区正在准备，完成后再试")
		return
	}
	defer w.prepareMu.Unlock()
	w.mu.Lock()
	workRoot := w.workRoot
	w.mu.Unlock()
	if workRoot == "" {
		workbenchError(out, 409, "请先设置全局工作目录")
		return
	}
	if _, err := validateWorkRoot(w.root, w.privateRoot, workRoot, workRoot); err != nil {
		workbenchError(out, 409, "已设置的工作目录不可用")
		return
	}
	if w.hasExistingPrecheck(r.Context(), id) {
		workbenchError(out, 409, "发现同编号任务或历史材料，请先核对现有执行")
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
		workbenchError(out, 409, "当前预检不是通过状态，不能开始新执行")
		return
	}
	if item.RequirementPrompt == "" || client.CanonicalPrompt(item.RequirementPrompt) != item.RequirementPrompt {
		workbenchError(out, 409, "平台题面原文无法冻结")
		return
	}
	language, err := w.startLanguage(r.Context(), item)
	if err != nil {
		workbenchError(out, 503, "仓库语言未核实，未创建任务："+err.Error())
		return
	}
	// The pull must survive the page being reloaded while a large repository
	// downloads: the mirror keeps the bytes, so an interrupted preparation
	// resumes instead of starting over.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), prepareTimeout)
	defer cancel()
	prepared, err := w.prepareIsolatedTask(ctx, item, language, workRoot)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			workbenchError(out, 409, fmt.Sprintf("准备超时（超过 %.0f 分钟）：仓库较大或网络较慢；已下载内容保留在项目缓存中，重试会接着下载", prepareTimeout.Minutes()))
			return
		}
		workbenchError(out, 409, err.Error())
		return
	}
	if _, err := w.runFlow(r.Context(), "precheck-bind", "-state", prepared.StatePath, "-item-id", strconv.Itoa(id), "-prompt", filepath.Join(filepath.Dir(prepared.StatePath), "PROMPT.md")); err != nil {
		workbenchError(out, 409, "源码和私有任务已创建，但预检绑定失败；请核查该任务，不能重新启动")
		return
	}
	state, err := client.NewWorkflowStore(prepared.StatePath).Load(r.Context())
	if err != nil {
		workbenchError(out, 409, "任务状态需要核查")
		return
	}
	planReady := w.prepareTaskPlan(r.Context(), prepared.StatePath, state) == nil
	writeWorkbenchJSON(out, 201, map[string]any{"task": w.taskView(r.Context(), prepared.StatePath), "plan_ready": planReady, "archived": prepared.Archived})
}

func (w *workbenchServer) executionDetails(out http.ResponseWriter, r *http.Request) {
	path, state, err := w.loadTask(r.Context(), r.PathValue("key"))
	if err != nil {
		workbenchError(out, 409, "任务审计链不可用")
		return
	}
	promptPath := filepath.Join(filepath.Dir(path), "PROMPT.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		workbenchError(out, 409, "冻结题面文件不可读")
		return
	}
	prompt := client.CanonicalPrompt(string(data))
	if client.TextSHA256(prompt) != state.Precheck.PromptSHA256 {
		workbenchError(out, 409, "题面与预检绑定不一致")
		return
	}
	writeWorkbenchJSON(out, 200, map[string]any{"workspace": state.Workspace, "baseline": state.Project.BaselineCommit, "prompt": prompt, "prompt_sha256": state.Precheck.PromptSHA256, "execution_mode": w.taskView(r.Context(), path).ExecutionMode, "execution_started_at": state.Trae.ExecutionStartedAt, "model": state.Trae.Model, "trace_ledger": traceLedger(path, state)})
}

func verifyPreparedSource(ctx context.Context, statePath string, state client.WorkflowState) error {
	receiptPath := filepath.Join(filepath.Dir(statePath), "source-preparation.json")
	if _, err := os.Lstat(receiptPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var receipt sourcePreparation
	if err := client.ReadLocalJSON(receiptPath, &receipt); err != nil {
		return fmt.Errorf("运行前源码回执不可读")
	}
	if receipt.SchemaVersion != 1 || receipt.Workspace != state.Workspace || receipt.RepoURL != state.Project.RepoURL || receipt.BaselineCommit != state.Project.BaselineCommit || receipt.PromptSHA256 != state.Precheck.PromptSHA256 || receipt.PrecheckID != state.Precheck.ItemID {
		return fmt.Errorf("运行前源码与预检绑定不一致")
	}
	head, files, hash, err := inspectFreshSource(ctx, state.Workspace, state.Project.RepoURL)
	if err != nil || head != receipt.BaselineCommit || hash != receipt.TrackedFilesSHA256 || len(files) != len(receipt.TrackedFiles) {
		return fmt.Errorf("源码已偏离准备时的干净基线")
	}
	return nil
}

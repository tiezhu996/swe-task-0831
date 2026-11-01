package main

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	client "example.com/swe-precheck-client"
)

type receiptCheckView struct {
	Key        string `json:"key,omitempty"`
	Name       string `json:"name,omitempty"`
	Category   string `json:"category,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Detail     string `json:"detail,omitempty"`
	DurationMS int    `json:"duration_ms,omitempty"`
}

type receiptView struct {
	Name       string             `json:"name"`
	Path       string             `json:"path"`
	Kind       string             `json:"kind"`
	Bytes      int64              `json:"bytes"`
	SHA256     string             `json:"sha256,omitempty"`
	ObservedAt string             `json:"observed_at,omitempty"`
	Status     string             `json:"status,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	Note       string             `json:"note,omitempty"`
	Checks     []receiptCheckView `json:"checks,omitempty"`
	Keys       []string           `json:"keys"`
	ReadState  string             `json:"read_state"`
}

type localRubricLine struct {
	ID   int    `json:"id"`
	OK   bool   `json:"ok"`
	Text string `json:"text"`
}

type localAssessment struct {
	Available bool              `json:"available"`
	Source    string            `json:"source,omitempty"`
	Total     int               `json:"total"`
	Passed    int               `json:"passed"`
	Failed    int               `json:"failed"`
	Lines     []localRubricLine `json:"lines,omitempty"`
	Note      string            `json:"note,omitempty"`
}

type evidenceConflict struct {
	Kind     string `json:"kind"`
	Local    string `json:"local"`
	Platform string `json:"platform"`
	Note     string `json:"note"`
}

// ---------- receipts ----------

func (w *workbenchServer) receipts(out http.ResponseWriter, r *http.Request) {
	path, state, err := w.loadTask(r.Context(), r.PathValue("key"))
	if err != nil {
		workbenchError(out, 409, "任务审计链不可用或任务不存在")
		return
	}
	taskDir := filepath.Dir(path)
	items, err := collectReceipts(taskDir, state)
	if err != nil {
		workbenchError(out, 409, "回执目录不可读")
		return
	}
	local := readLocalAssessment(state)
	platformReason, platformStatus := platformConclusion(items)
	conflicts := receiptConflicts(local, platformReason, string(state.Submission.Status))
	kinds := map[string]int{}
	for _, item := range items {
		kinds[item.Kind]++
	}
	writeWorkbenchJSON(out, 200, map[string]any{
		"task_id":         state.TaskID,
		"total":           len(items),
		"kinds":           kinds,
		"items":           items,
		"local":           local,
		"platform_reason": platformReason,
		"platform_status": platformStatus,
		"conflicts":       conflicts,
	})
}

func collectReceipts(taskDir string, state client.WorkflowState) ([]receiptView, error) {
	seen := map[string]bool{}
	paths := []string{}
	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		if !pathContains(taskDir, path) {
			return
		}
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
			return
		}
		seen[path] = true
		paths = append(paths, path)
	}
	add(state.Submission.ReceiptPath)
	err := filepath.WalkDir(taskDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if path == taskDir {
				return nil
			}
			name := entry.Name()
			if name == ".git" || name == "sources" || name == "node_modules" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			rel, relErr := filepath.Rel(taskDir, path)
			if relErr == nil && strings.Count(rel, string(os.PathSeparator)) >= 2 {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		name := strings.ToLower(entry.Name())
		if !strings.HasSuffix(name, ".json") || !strings.Contains(name, "receipt") {
			return nil
		}
		add(path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if len(paths) > receiptMaxFiles {
		paths = paths[:receiptMaxFiles]
	}
	items := make([]receiptView, 0, len(paths))
	for _, path := range paths {
		items = append(items, readReceipt(path))
	}
	return items, nil
}

func readReceipt(path string) receiptView {
	view := receiptView{Name: filepath.Base(path), Path: path, Kind: "unknown", ReadState: "ok", Keys: []string{}}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		view.ReadState = "unreadable"
		return view
	}
	view.Bytes = info.Size()
	if info.Size() > receiptMaxBytes {
		view.ReadState = "truncated"
		return view
	}
	data, err := os.ReadFile(path)
	if err != nil {
		view.ReadState = "unreadable"
		return view
	}
	view.SHA256 = hex.EncodeToString(sum256(data))
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		view.ReadState = "unreadable"
		return view
	}
	for key := range payload {
		view.Keys = append(view.Keys, key)
	}
	sort.Strings(view.Keys)
	view.Kind = classifyReceipt(payload)
	view.Status = firstString(payload, "status", "platform_status", "created_status", "outcome", "result")
	view.ObservedAt = firstString(payload, "updated_at", "observed_at", "queried_at", "checked_at", "submitted_at", "collected_at", "created_at")
	view.Reason = receiptReason(payload)
	view.Note = digString(payload, "repair_note")
	view.Checks = receiptChecks(payload)
	return view
}

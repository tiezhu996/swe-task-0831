package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	client "example.com/swe-precheck-client"
)

// ---------- journal ----------

type journalEvent struct {
	ID           string                `json:"event_id"`
	At           string                `json:"at"`
	PreviousHash string                `json:"previous_sha256,omitempty"`
	Hash         string                `json:"sha256"`
	From         client.WorkflowStatus `json:"from,omitempty"`
	To           client.WorkflowStatus `json:"to"`
	Audit        client.WorkflowAudit  `json:"audit"`
	State        client.WorkflowState  `json:"state"`
}

type journalEventView struct {
	EventID       string `json:"event_id"`
	At            string `json:"at"`
	From          string `json:"from,omitempty"`
	To            string `json:"to"`
	Action        string `json:"action,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
	Revision      int64  `json:"revision"`
	ExternalWrite bool   `json:"external_write"`
}

type journalChainView struct {
	Verified    bool   `json:"verified"`
	Count       int    `json:"count"`
	BrokenAt    int    `json:"broken_at,omitempty"`
	Reason      string `json:"reason,omitempty"`
	LastSHA256  string `json:"last_sha256,omitempty"`
	HeadMatches bool   `json:"head_matches"`
}

type attachmentView struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	Exists    bool   `json:"exists"`
	HashMatch bool   `json:"hash_match,omitempty"`
}

func (w *workbenchServer) journal(out http.ResponseWriter, r *http.Request) {
	path, state, err := w.loadTask(r.Context(), r.PathValue("key"))
	if err != nil {
		workbenchError(out, 409, "任务审计链不可用或任务不存在")
		return
	}
	full := r.URL.Query().Get("full") == "1"
	events, chain, err := readJournal(path, state)
	if err != nil {
		workbenchError(out, 409, "任务事件链不可读")
		return
	}
	attachments := taskAttachments(path, state, full)
	response := map[string]any{
		"task_id":     state.TaskID,
		"status":      string(state.Status),
		"revision":    state.Revision,
		"archived":    state.Archived,
		"updated_at":  state.UpdatedAt,
		"chain":       chain,
		"attachments": attachments,
	}
	if full {
		response["state"] = state
		response["raw_events"] = events
	} else {
		limit := 50
		start := 0
		if len(events) > limit {
			start = len(events) - limit
		}
		views := make([]journalEventView, 0, len(events)-start)
		for _, event := range events[start:] {
			views = append(views, journalEventView{
				EventID:       event.ID,
				At:            event.At,
				From:          string(event.From),
				To:            string(event.To),
				Action:        event.Audit.Action,
				Outcome:       event.Audit.Outcome,
				Revision:      event.State.Revision,
				ExternalWrite: event.Audit.ExternalWrite,
			})
		}
		response["events"] = views
		response["events_total"] = len(events)
	}
	writeWorkbenchJSON(out, 200, response)
}

func readJournal(path string, state client.WorkflowState) ([]journalEvent, journalChainView, error) {
	chain := journalChainView{HeadMatches: false}
	info, err := os.Lstat(filepath.Join(filepath.Dir(path), "events.jsonl"))
	if err != nil {
		return nil, chain, err
	}
	if !info.Mode().IsRegular() || info.Size() > journalMaxBytes {
		return nil, chain, errors.New("journal unavailable")
	}
	file, err := os.Open(filepath.Join(filepath.Dir(path), "events.jsonl"))
	if err != nil {
		return nil, chain, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<20)
	events := []journalEvent{}
	previousHash := ""
	var previousRevision int64
	previousStatus := client.WorkflowStatus("")
	verified := true
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var event journalEvent
				if json.Unmarshal(trimmed, &event) != nil {
					if verified {
						chain.BrokenAt = len(events) + 1
						chain.Reason = "事件 JSON 无法解析"
					}
					verified = false
				} else {
					events = append(events, event)
					if verified {
						if reason := verifyJournalEvent(event, previousHash, previousRevision, previousStatus); reason != "" {
							verified = false
							chain.BrokenAt = len(events)
							chain.Reason = reason
						}
					}
					previousHash = event.Hash
					previousRevision = event.State.Revision
					previousStatus = event.To
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, chain, readErr
		}
	}
	chain.Count = len(events)
	chain.Verified = verified && len(events) > 0
	if len(events) > 0 {
		chain.LastSHA256 = events[len(events)-1].Hash
		chain.HeadMatches = state.LastEventSHA256 == chain.LastSHA256
	}
	if chain.Verified && !chain.HeadMatches {
		chain.Verified = false
		chain.Reason = "最后一个事件与任务快照不一致"
	}
	return events, chain, nil
}

func verifyJournalEvent(event journalEvent, previousHash string, previousRevision int64, previousStatus client.WorkflowStatus) string {
	hash := event.Hash
	event.Hash = ""
	encoded, _ := json.Marshal(event)
	event.Hash = hash
	expected := hex.EncodeToString(sum256(encoded))
	if hash != expected {
		return "事件哈希不匹配"
	}
	if event.PreviousHash != previousHash {
		return "previous_sha256 与上一事件不衔接"
	}
	if event.State.Revision != previousRevision+1 {
		return "revision 不连续"
	}
	if event.From != previousStatus {
		return "from 与上一事件状态不一致"
	}
	if event.To != event.State.Status {
		return "to 与 state.status 不一致"
	}
	if event.State.LastEventSHA256 != "" {
		return "事件内嵌的 last_event_sha256 应为空"
	}
	return ""
}

func taskAttachments(path string, state client.WorkflowState, verify bool) []attachmentView {
	taskDir := filepath.Dir(path)
	items := []attachmentView{}
	add := func(artifact client.WorkflowArtifact) {
		view := attachmentView{Path: artifact.Path, SHA256: artifact.SHA256, Bytes: artifact.Bytes}
		if artifact.Path == "" || !pathContains(taskDir, artifact.Path) {
			items = append(items, view)
			return
		}
		info, err := os.Lstat(artifact.Path)
		view.Exists = err == nil && info.Mode().IsRegular()
		if view.Exists && verify && info.Size() <= traceMaxBytes {
			if data, err := os.ReadFile(artifact.Path); err == nil {
				view.HashMatch = hex.EncodeToString(sum256(data)) == artifact.SHA256
			}
		}
		items = append(items, view)
	}
	keys := make([]string, 0, len(state.Verification.Artifacts))
	for key := range state.Verification.Artifacts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		add(state.Verification.Artifacts[key])
	}
	if state.Verification.DesignPath != "" {
		add(client.WorkflowArtifact{Path: state.Verification.DesignPath, SHA256: state.Verification.DesignSHA256})
	}
	return items
}

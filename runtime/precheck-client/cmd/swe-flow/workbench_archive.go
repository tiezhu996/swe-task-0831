package main

import (
	"net/http"
	"strconv"

	client "example.com/swe-precheck-client"
)

// setTaskArchived shelves or restores one local task without changing its
// workflow status, so a task at any stage can leave the active queue and come
// back exactly as it was. The toggle goes through the same expected-revision
// update as every other mutation, which keeps the journal and its audit trail
// complete.
func (w *workbenchServer) setTaskArchived(out http.ResponseWriter, r *http.Request) {
	w.evidenceMu.Lock()
	defer w.evidenceMu.Unlock()
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	archived, ok := decodeArchiveIntent(out, r)
	if !ok {
		return
	}
	action := "archive"
	if !archived {
		action = "unarchive"
	}
	if _, err := client.NewWorkflowStore(path).Update(r.Context(), state.Revision, client.WorkflowAudit{Action: action, Outcome: "success"}, func(next *client.WorkflowState) error {
		next.Archived = archived
		return nil
	}); err != nil {
		workbenchError(out, 409, err.Error())
		return
	}
	writeWorkbenchJSON(out, 200, w.taskView(r.Context(), path))
}

// setPrecheckArchived shelves a platform precheck that never became a local
// task. The platform has no such flag, so the shelf list belongs to the
// workbench settings and survives restarts.
func (w *workbenchServer) setPrecheckArchived(out http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		workbenchError(out, 400, "预检编号无效")
		return
	}
	archived, ok := decodeArchiveIntent(out, r)
	if !ok {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	next := make([]int, 0, len(w.archivedPrechecks)+1)
	for _, existing := range w.archivedPrechecks {
		if existing != id {
			next = append(next, existing)
		}
	}
	if archived {
		next = append(next, id)
	}
	previous := w.archivedPrechecks
	w.archivedPrechecks = next
	if err := w.saveSettingsLocked(); err != nil {
		w.archivedPrechecks = previous
		workbenchError(out, 500, "归档状态保存失败")
		return
	}
	writeWorkbenchJSON(out, 200, map[string]any{"id": id, "archived": archived})
}

// decodeArchiveIntent requires an explicit target state: one endpoint both
// shelves and restores, so a missing flag must never pick a direction.
func decodeArchiveIntent(out http.ResponseWriter, r *http.Request) (archived bool, ok bool) {
	var input struct {
		Archived *bool `json:"archived"`
	}
	if decodeWorkbenchJSON(r, &input) != nil || input.Archived == nil {
		workbenchError(out, 400, "需要明确归档或恢复的意图")
		return false, false
	}
	return *input.Archived, true
}

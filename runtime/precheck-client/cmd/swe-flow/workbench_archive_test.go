package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	client "example.com/swe-precheck-client"
)

// archivedTaskServer builds a workbench over one initialized task so the
// handler can be driven directly, without a listening port.
func archivedTaskServer(t *testing.T) (*workbenchServer, string, string) {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "sources", "demo")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "workflow", "demo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "task-state.json")
	if _, err := client.NewWorkflowStore(path).Init(context.Background(), "demo", workspace); err != nil {
		t.Fatal(err)
	}
	w := &workbenchServer{root: root, workRoot: root, privateRoot: filepath.Join(root, ".private")}
	return w, path, workbenchTaskKey(path)
}

func archiveRequest(t *testing.T, w *workbenchServer, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/"+key+"/archive", strings.NewReader(body))
	req.SetPathValue("key", key)
	rec := httptest.NewRecorder()
	w.setTaskArchived(rec, req)
	return rec
}

func TestArchiveShelvesTaskWithoutChangingStatus(t *testing.T) {
	ctx := context.Background()
	w, path, key := archivedTaskServer(t)
	before, err := client.NewWorkflowStore(path).Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rec := archiveRequest(t, w, key, `{"archived":true}`); rec.Code != http.StatusOK {
		t.Fatalf("archive status=%d body=%s", rec.Code, rec.Body.String())
	}
	archived, err := client.NewWorkflowStore(path).Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The shelf must never rewrite the workflow: only the flag and the revision
	// move, so every later transition still applies unchanged.
	if !archived.Archived || archived.Status != before.Status || archived.Revision != before.Revision+1 {
		t.Fatalf("archived state = %+v (before %+v)", archived, before)
	}
	if rec := archiveRequest(t, w, key, `{"archived":false}`); rec.Code != http.StatusOK {
		t.Fatalf("restore status=%d body=%s", rec.Code, rec.Body.String())
	}
	restored, err := client.NewWorkflowStore(path).Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Archived || restored.Status != before.Status || restored.Revision != before.Revision+2 {
		t.Fatalf("restored state = %+v", restored)
	}
}

func TestArchiveRequiresExplicitIntent(t *testing.T) {
	w, _, key := archivedTaskServer(t)
	for _, body := range []string{`{}`, `{"archived":null}`} {
		if rec := archiveRequest(t, w, key, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s status=%d", body, rec.Code)
		}
	}
}

func TestArchivedPrecheckSurvivesSettingsReload(t *testing.T) {
	root := t.TempDir()
	w := &workbenchServer{root: root, workRoot: root, privateRoot: filepath.Join(root, ".private")}
	req := httptest.NewRequest(http.MethodPost, "/api/prechecks/42/archive", strings.NewReader(`{"archived":true}`))
	req.SetPathValue("id", "42")
	rec := httptest.NewRecorder()
	w.setPrecheckArchived(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive status=%d body=%s", rec.Code, rec.Body.String())
	}
	// A restart reads the same shelf back from the private settings file.
	reloaded := &workbenchServer{root: root, workRoot: root, privateRoot: filepath.Join(root, ".private")}
	if err := reloaded.loadSettings(); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.archivedPrechecks) != 1 || reloaded.archivedPrechecks[0] != 42 {
		t.Fatalf("archived prechecks = %v", reloaded.archivedPrechecks)
	}
	restore := httptest.NewRequest(http.MethodPost, "/api/prechecks/42/archive", strings.NewReader(`{"archived":false}`))
	restore.SetPathValue("id", "42")
	rec = httptest.NewRecorder()
	reloaded.setPrecheckArchived(rec, restore)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(reloaded.archivedPrechecks) != 0 {
		t.Fatalf("archived prechecks after restore = %v", reloaded.archivedPrechecks)
	}
}

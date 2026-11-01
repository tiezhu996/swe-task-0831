package main

import (
	"context"
	client "example.com/swe-precheck-client"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrecheckCacheWarmReadAndRestartNeedNoPlatformCalls(t *testing.T) {
	root := t.TempDir()
	setupWorkbenchTask(t, root, "one", "https://github.com/example/repo", "冻结题面")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		out.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/precheck/items":
			fmt.Fprint(out, `{"code":0,"data":{"items":[{"id":12,"repo_url":"https://github.com/example/repo","status":"PRECHECK_PASSED"}],"page":1,"page_size":100,"total":1}}`)
		case "/precheck/items/12":
			fmt.Fprint(out, `{"code":0,"data":{"id":12,"repo_url":"https://github.com/example/repo","requirement_prompt":"冻结题面"}}`)
		default:
			http.NotFound(out, r)
		}
	}))
	defer server.Close()
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	w.newClient = func(context.Context) (*client.Client, error) { return client.New(server.URL, nil) }
	get := func(w *workbenchServer, url string) *httptest.ResponseRecorder {
		out := httptest.NewRecorder()
		w.handler().ServeHTTP(out, httptest.NewRequest("GET", url, nil))
		return out
	}
	first := get(w, "/api/prechecks?refresh=1")
	if first.Code != 200 || calls.Load() != 2 {
		t.Fatalf("initial calls=%d status=%d body=%s", calls.Load(), first.Code, first.Body.String())
	}
	w.newClient = func(context.Context) (*client.Client, error) {
		t.Error("warm cache authenticated")
		return nil, fmt.Errorf("offline")
	}
	for i := 0; i < 3; i++ {
		out := get(w, "/api/prechecks")
		if out.Code != 200 || !strings.Contains(out.Body.String(), `"matches":["`) {
			t.Fatalf("warm=%d %s", out.Code, out.Body.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatal("warm read used platform")
	}
	cachePath := filepath.Join(w.privateRoot, "prechecks-cache.json")
	info, err := os.Stat(cachePath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("cache permissions: %v", err)
	}
	restarted, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	restarted.newClient = func(context.Context) (*client.Client, error) {
		t.Error("restarted cache authenticated")
		return nil, fmt.Errorf("offline")
	}
	out := get(restarted, "/api/prechecks")
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"matches":["`) {
		t.Fatalf("restart %d %s", out.Code, out.Body.String())
	}
}

func TestPrecheckCacheReturnsDuringRefreshAndPreservesFailedSnapshot(t *testing.T) {
	w, err := newWorkbenchServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w.prechecks = []client.BatchItem{{ID: 1, RepoURL: "https://github.com/o/r", Status: "PRECHECK_PASSED"}}
	w.prechecksAt = time.Now().Add(-10 * time.Minute)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	w.newClient = func(context.Context) (*client.Client, error) {
		calls.Add(1)
		close(entered)
		<-release
		return nil, fmt.Errorf("offline")
	}
	w.refreshPrecheckCacheIfDue()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("background refresh not started")
	}
	complete := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		out := httptest.NewRecorder()
		w.handler().ServeHTTP(out, httptest.NewRequest("GET", "/api/prechecks", nil))
		complete <- out
	}()
	select {
	case out := <-complete:
		if out.Code != 200 || !strings.Contains(out.Body.String(), `"refreshing":true`) {
			t.Fatalf("stale cache %d %s", out.Code, out.Body.String())
		}
	case <-time.After(500 * time.Millisecond):
		close(release)
		t.Fatal("warm read waited for network")
	}
	close(release)
	w.precheckMu.Lock()
	w.precheckMu.Unlock()
	out := httptest.NewRecorder()
	w.handler().ServeHTTP(out, httptest.NewRequest("GET", "/api/prechecks", nil))
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"sync_error":true`) || !strings.Contains(out.Body.String(), `"id":1`) {
		t.Fatalf("failed sync erased cache: %s", out.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatal("failed sync was retried without backoff")
	}
}

func TestPrecheckCacheRejectsCorruptOrPublicSnapshots(t *testing.T) {
	root := t.TempDir()
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.privateRoot, "prechecks-cache.json")
	snapshot := persistedPrecheckCache{SchemaVersion: 1, Root: w.root, SyncedAt: time.Now(), Items: []client.BatchItem{}}
	if err := client.SaveLocalJSON(path, snapshot); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0644)
	restart, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	if !restart.prechecksAt.IsZero() {
		t.Fatal("public cache loaded")
	}
	_ = os.Chmod(path, 0600)
	_ = os.WriteFile(path, []byte("broken"), 0600)
	restart, err = newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	if !restart.prechecksAt.IsZero() {
		t.Fatal("corrupt cache loaded")
	}
}

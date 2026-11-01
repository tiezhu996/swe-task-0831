package precheckclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoginStoresCookiesAndSendsCSRF(t *testing.T) {
	var csrfSeen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/login":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["username"] != "u" {
				t.Fatalf("bad login body: %#v, %v", body, err)
			}
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "opaque-session", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: "csrf-value", Path: "/"})
			_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"username":"u"}}`))
		case "/precheck/batches":
			csrfSeen = r.Header.Get("X-CSRF-Token")
			_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"batch_id":"batch-1","accepted":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c, err := New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Login(context.Background(), "u", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.SubmitBatch(context.Background(), []Row{{RepoURL: "https://github.com/a/b", Prompt: "prompt"}}); err != nil {
		t.Fatal(err)
	}
	if csrfSeen != "csrf-value" {
		t.Fatalf("csrf header = %q", csrfSeen)
	}
}

func TestSetSessionCookies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie(sessionCookie); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"username":"u"}}`))
	}))
	defer server.Close()
	c, err := New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetSessionCookies("opaque-session", "csrf-value")
	if _, err = c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteSubmissionDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodDelete || r.URL.Path != "/submissions/1480" {
			t.Errorf("unexpected delete request")
		}
		if r.Header.Get("X-CSRF-Token") != "test-csrf" {
			t.Error("missing CSRF protection")
		}
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c, _ := New(server.URL, nil)
	c.SetSessionCookies("test-session", "test-csrf")
	if _, err := c.DeleteSubmission(context.Background(), 1480); err == nil {
		t.Fatal("expected delete failure")
	}
	if calls.Load() != 1 {
		t.Fatalf("delete retried %d times", calls.Load())
	}
}

func TestReadRequestsRetryTransientServerErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"username":"u"}}`))
	}))
	defer server.Close()

	c, err := NewWithRetry(server.URL, nil, RetryPolicy{
		MaxAttempts:  3,
		InitialDelay: time.Millisecond,
		MaxDelay:     time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	user, err := c.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "u" || calls.Load() != 3 {
		t.Fatalf("user=%#v calls=%d", user, calls.Load())
	}
}

func TestMutatingRequestsAreNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	c, err := NewWithRetry(server.URL, nil, RetryPolicy{
		MaxAttempts:  3,
		InitialDelay: time.Millisecond,
		MaxDelay:     time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SubmitBatch(context.Background(), []Row{{RepoURL: "https://github.com/a/b", Prompt: "prompt"}})
	if err == nil {
		t.Fatal("expected submit error")
	}
	if calls.Load() != 1 {
		t.Fatalf("mutating request was retried: calls=%d", calls.Load())
	}
}

func TestWaitBatchStopsAtTerminalState(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := "running"
		finished := false
		if calls.Add(1) >= 2 {
			status = "completed"
			finished = true
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"batch":{"batch_id":"b-1","status":"` + status + `","finished":` + map[bool]string{true: "true", false: "false"}[finished] + `}}}`))
	}))
	defer server.Close()

	c, err := NewWithRetry(server.URL, nil, RetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	progress, err := c.WaitBatch(context.Background(), "b-1", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !progress.Batch.Finished || progress.Batch.Status != "completed" || calls.Load() != 2 {
		t.Fatalf("progress=%#v calls=%d", progress.Batch, calls.Load())
	}
}

func TestWaitBatchWithOptionsReportsProgress(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := "running"
		finished := "false"
		if calls.Add(1) >= 2 {
			status = "finished"
			finished = "true"
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"batch":{"batch_id":"b-2","status":"` + status + `","finished":` + finished + `}}}`))
	}))
	defer server.Close()

	c, err := NewWithRetry(server.URL, nil, RetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	progress, err := c.WaitBatchWithOptions(context.Background(), "b-2", WaitOptions{
		InitialInterval: time.Millisecond,
		MaxInterval:     time.Millisecond,
		BackoffFactor:   1,
		OnProgress: func(p BatchProgress) {
			seen = append(seen, p.Batch.Status)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if progress.Batch.Status != "finished" || len(seen) != 2 || seen[0] != "running" || seen[1] != "finished" {
		t.Fatalf("progress=%#v seen=%v", progress.Batch, seen)
	}
}

func TestListAllItemsRejectsStalledPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"items":[{"id":1}],"page":` + r.URL.Query().Get("page") + `,"page_size":1,"total":3}}`))
	}))
	defer server.Close()

	c, err := NewWithRetry(server.URL, nil, RetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListAllItems(context.Background(), ItemQuery{PageSize: 1})
	if err == nil || !strings.Contains(err.Error(), "pagination did not advance") {
		t.Fatalf("expected pagination guard, got %v", err)
	}
}

func TestHTTPErrorPreservesStatusCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"login required"}`))
	}))
	defer server.Close()

	c, err := NewWithRetry(server.URL, nil, RetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Me(context.Background())
	var apiErr *HTTPError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized || apiErr.Detail != "login required" {
		t.Fatalf("error=%v typed=%#v", err, apiErr)
	}
}

func TestRequestTimeoutBoundsSingleAttempt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	c, err := NewWithRetry(server.URL, nil, RetryPolicy{MaxAttempts: 1, RequestTimeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Me(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected request deadline, got %v", err)
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	delay := retryAfterDelay(now.Add(2*time.Second).Format(http.TimeFormat), now)
	if delay != 2*time.Second {
		t.Fatalf("delay=%s", delay)
	}
}

func TestBatchStateRoundTripIsLocalAndPrivate(t *testing.T) {
	rows := []Row{{RepoURL: "https://github.com/a/b", Prompt: "prompt"}}
	created := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	state, err := NewBatchState(SubmitResult{BatchID: "batch-42"}, rows, created)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "nested", "batch.json")
	if err := SaveBatchState(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBatchState(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != state {
		t.Fatalf("loaded=%#v state=%#v", loaded, state)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state permissions=%o", info.Mode().Perm())
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "password") || strings.Contains(string(content), "csrf") {
		t.Fatal("state unexpectedly contains credentials")
	}
}

func TestBatchStateRejectsMissingBatchID(t *testing.T) {
	if _, err := NewBatchState(SubmitResult{}, nil, time.Time{}); err == nil {
		t.Fatal("expected missing batch ID error")
	}
	path := filepath.Join(t.TempDir(), "batch.json")
	if err := os.WriteFile(path, []byte(`{"rows_sha256":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBatchState(path); err == nil {
		t.Fatal("expected invalid state error")
	}
}

func TestUploadAttachmentUsesMultipartAndCSRF(t *testing.T) {
	var csrfSeen string
	var content string
	var filename string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/uploads/attachment" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		csrfSeen = r.Header.Get("X-CSRF-Token")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("file part: %v", err)
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		content, filename = string(data), header.Filename
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"name":"trace.md","url":"/uploads/trace.md"}}`))
	}))
	defer server.Close()

	c, err := New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetSessionCookies("session", "csrf-upload")
	path := filepath.Join(t.TempDir(), "trace.md")
	if err := os.WriteFile(path, []byte("real trace"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := c.UploadAttachment(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "trace.md" || result.URL != "/uploads/trace.md" || csrfSeen != "csrf-upload" || content != "real trace" || filename != "trace.md" {
		t.Fatalf("result=%#v csrf=%q content=%q filename=%q", result, csrfSeen, content, filename)
	}
}

func TestCreateSubmissionSendsPayloadSchemaAndPatchWithoutRemarks(t *testing.T) {
	var csrfSeen string
	var payload map[string]any
	var schemaVersion string
	var patchContent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/submissions/create" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		csrfSeen = r.Header.Get("X-CSRF-Token")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		if err := json.Unmarshal([]byte(r.FormValue("payload")), &payload); err != nil {
			t.Fatalf("payload json: %v", err)
		}
		schemaVersion = r.FormValue("schema_version")
		file, _, err := r.FormFile("patch_file")
		if err != nil {
			t.Fatalf("patch part: %v", err)
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		patchContent = string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"id":17,"status":"submitted"}}`))
	}))
	defer server.Close()

	c, err := New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetSessionCookies("session", "csrf-submit")
	patchPath := filepath.Join(t.TempDir(), "change.patch")
	if err := os.WriteFile(patchPath, []byte("diff --git"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := c.CreateSubmission(context.Background(), map[string]any{
		"precheck_item_id": 2410,
		"repo_url":         "https://github.com/example/repo",
		"effective_rounds": 45,
	}, patchPath, "1")
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != 17 || result.Status != "submitted" || csrfSeen != "csrf-submit" || schemaVersion != "1" || patchContent != "diff --git" {
		t.Fatalf("result=%#v csrf=%q schema=%q patch=%q", result, csrfSeen, schemaVersion, patchContent)
	}
	if payload["precheck_item_id"] != float64(2410) || payload["repo_url"] != "https://github.com/example/repo" {
		t.Fatalf("payload=%#v", payload)
	}
	if _, ok := payload["备注"]; ok {
		t.Fatal("remarks unexpectedly sent")
	}
}

func TestCreateSubmissionRejectsInternalFieldsBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer server.Close()
	c, err := New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"备注", "remark", "REMARKS", "note", "notes", "patch_file"} {
		_, err := c.CreateSubmission(context.Background(), map[string]any{key: "internal"}, "", "")
		if err == nil || !strings.Contains(err.Error(), "forbidden internal field") {
			t.Fatalf("key=%q err=%v", key, err)
		}
	}
	_, err = c.CreateSubmission(context.Background(), map[string]any{
		"metadata": []any{map[string]any{"备注": "internal"}},
	}, "", "")
	if err == nil || !strings.Contains(err.Error(), "forbidden internal field") {
		t.Fatalf("nested forbidden field err=%v", err)
	}
	for _, value := range []any{map[string]string{"备注": "internal"}, json.RawMessage(`{"notes":"internal"}`)} {
		_, err := c.CreateSubmission(context.Background(), map[string]any{"metadata": value}, "", "")
		if err == nil || !strings.Contains(err.Error(), "forbidden internal field") {
			t.Fatalf("typed nested value bypassed guard: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("server called %d times", calls.Load())
	}
}

func TestMultipartWritesAreNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c, err := NewWithRetry(server.URL, nil, RetryPolicy{MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "trace.md")
	if err := os.WriteFile(path, []byte("trace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UploadAttachment(context.Background(), path); err == nil {
		t.Fatal("expected upload failure")
	}
	if _, err := c.CreateSubmission(context.Background(), map[string]any{"title": "task"}, "", ""); err == nil {
		t.Fatal("expected submission failure")
	}
	if calls.Load() != 2 {
		t.Fatalf("writes were retried: %d", calls.Load())
	}
}

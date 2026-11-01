package precheckclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPlanRejectsSchemaDriftAndBadAttachments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.md")
	if err := os.WriteFile(path, []byte("trace"), 0o600); err != nil {
		t.Fatal(err)
	}
	schema := FormSchema{Version: "v1", Fields: []FormField{
		{Key: "precheck_item_id", Type: "precheck_ref"},
		{Key: "primary_language", Type: "select", Required: true, Options: []string{"Go"}},
		{Key: "x_trae_trace", Type: "attachment", Required: true},
		{Key: "submitter_name", Type: "text", Auto: true},
	}}
	schema.Attachment.Configured = true
	schema.Attachment.MaxMB = 20
	schema.Attachment.Accept = ".md"
	plan := SubmissionPlan{SchemaVersion: "v1", ExecutionPrompt: "actual prompt", Payload: map[string]any{"precheck_item_id": 2410, "primary_language": "Go"}, Attachments: map[string]string{"x_trae_trace": path}}
	if err := ValidateSubmissionPlan(plan, schema); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, expected string
		change         func(*SubmissionPlan)
	}{
		{"schema drift", "version", func(p *SubmissionPlan) { p.SchemaVersion = "old" }},
		{"remarks", "forbidden", func(p *SubmissionPlan) { p.Payload["备注"] = "local" }},
		{"auto field", "platform-owned", func(p *SubmissionPlan) { p.Payload["submitter_name"] = "user" }},
		{"bad enum", "selection", func(p *SubmissionPlan) { p.Payload["primary_language"] = "Rust" }},
		{"missing precheck", "precheck_item_id", func(p *SubmissionPlan) { delete(p.Payload, "precheck_item_id") }},
		{"missing trace", "attachment", func(p *SubmissionPlan) { delete(p.Attachments, "x_trae_trace") }},
		{"file unavailable", "unavailable", func(p *SubmissionPlan) { p.Attachments["x_trae_trace"] = path + ".missing" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			copy := SubmissionPlan{SchemaVersion: plan.SchemaVersion, ExecutionPrompt: plan.ExecutionPrompt, Payload: map[string]any{}, Attachments: map[string]string{}}
			for k, v := range plan.Payload {
				copy.Payload[k] = v
			}
			for k, v := range plan.Attachments {
				copy.Attachments[k] = v
			}
			tt.change(&copy)
			if err := ValidateSubmissionPlan(copy, schema); err == nil || !strings.Contains(err.Error(), tt.expected) {
				t.Fatalf("expected %q, got %v", tt.expected, err)
			}
		})
	}
	p, s := submissionFixture(t)
	s.Limited = true
	if err := ValidateSubmissionPlan(p, s); err == nil || !strings.Contains(err.Error(), "platform currently blocks") {
		t.Fatalf("limited schema should block final submission validation, got %v", err)
	}
}

// All identifiers here are synthetic, never copied from a real session.
func syntheticToken() string {
	return strings.Repeat("a", 16) + ":" + strings.Repeat("b", 32) + "_" + strings.Repeat("c", 24) + "." + strings.Repeat("d", 24) + "." + strings.Repeat("e", 24) + ":TraeCode CN.3.3.90.no_sid.no_ppe.T(2026/10/5 20:14:45)"
}

func TestValidateTraeSessionTokenAcceptsCurrentTraeCNLabel(t *testing.T) {
	legacy := syntheticToken()
	current := strings.Replace(legacy, ":TraeCode CN.3.3.90.", ":TRAE CN.3.4.7.", 1)
	if current == legacy {
		t.Fatal("fixture did not switch the product label")
	}
	for _, token := range []string{legacy, current} {
		if err := ValidateTraeSessionToken(token); err != nil {
			t.Fatalf("valid Trae CN token rejected: %v", err)
		}
	}
	broken := []string{
		strings.TrimSuffix(current, "T(2026/10/5 20:14:45)"),
		strings.Replace(current, ":TRAE CN.", ":TraeWork.", 1),
		strings.Replace(current, strings.Repeat("a", 16), "…", 1),
	}
	for _, token := range broken {
		if err := ValidateTraeSessionToken(token); err == nil {
			t.Fatalf("incomplete token accepted: %q", token)
		}
	}
}

func submissionFixture(t *testing.T) (SubmissionPlan, FormSchema) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.md")
	if err := os.WriteFile(path, []byte("synthetic trace"), 0o600); err != nil {
		t.Fatal(err)
	}
	schema := FormSchema{Version: "v1", Fields: []FormField{
		{Key: "precheck_item_id", Type: "precheck_ref"},
		{Key: "repo_url", Type: "url", Required: true},
		{Key: "title", Type: "text", Required: true},
		{Key: "trae_session_id", Type: "text", Required: true},
		{Key: "effective_rounds", Type: "number", Required: true},
		{Key: "x_trae_trace", Type: "attachment", Required: true},
		{Key: "requirement_prompt", Type: "textarea", Required: true, Auto: true},
	}}
	schema.Attachment.Configured = true
	schema.Attachment.Accept = ".md"
	return SubmissionPlan{SchemaVersion: "v1", ExecutionPrompt: "actual prompt", Payload: map[string]any{
		"precheck_item_id": 2409, "repo_url": "https://github.com/example/repo", "title": "task", "trae_session_id": syntheticToken(), "effective_rounds": 120,
	}, Attachments: map[string]string{"x_trae_trace": path}}, schema
}

func TestRequiredFieldAuditBeforeAnyNetworkWrite(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		t.Errorf("invalid plan reached network: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", 400)
	}))
	defer server.Close()
	c, _ := New(server.URL, nil)
	cases := []struct {
		name   string
		change func(*SubmissionPlan, *FormSchema)
	}{
		{"missing session", func(p *SubmissionPlan, s *FormSchema) { delete(p.Payload, "trae_session_id") }},
		{"blank session", func(p *SubmissionPlan, s *FormSchema) { p.Payload["trae_session_id"] = " " }},
		{"placeholder session", func(p *SubmissionPlan, s *FormSchema) { p.Payload["trae_session_id"] = "未提供" }},
		{"short session", func(p *SubmissionPlan, s *FormSchema) { p.Payload["trae_session_id"] = strings.Repeat("a", 24) }},
		{"truncated session", func(p *SubmissionPlan, s *FormSchema) { p.Payload["trae_session_id"] = syntheticToken()[:120] }},
		{"numeric session", func(p *SubmissionPlan, s *FormSchema) { p.Payload["trae_session_id"] = 123 }},
		{"missing title", func(p *SubmissionPlan, s *FormSchema) { delete(p.Payload, "title") }},
		{"placeholder title", func(p *SubmissionPlan, s *FormSchema) { p.Payload["title"] = "待填写" }},
		{"object title", func(p *SubmissionPlan, s *FormSchema) { p.Payload["title"] = map[string]any{"value": "task"} }},
		{"bad URL", func(p *SubmissionPlan, s *FormSchema) { p.Payload["repo_url"] = "javascript:alert(1)" }},
		{"fractional precheck", func(p *SubmissionPlan, s *FormSchema) { p.Payload["precheck_item_id"] = 1.5 }},
		{"fractional rounds", func(p *SubmissionPlan, s *FormSchema) { p.Payload["effective_rounds"] = 45.5 }},
		{"missing attachment", func(p *SubmissionPlan, s *FormSchema) { delete(p.Attachments, "x_trae_trace") }},
		{"empty attachment", func(p *SubmissionPlan, s *FormSchema) {
			if err := os.WriteFile(p.Attachments["x_trae_trace"], nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"required file", func(p *SubmissionPlan, s *FormSchema) {
			s.Fields = append(s.Fields, FormField{Key: "patch_file", Type: "file", Required: true})
		}},
		{"remarks", func(p *SubmissionPlan, s *FormSchema) { p.Payload["备注"] = "local only" }},
		{"schema drift", func(p *SubmissionPlan, s *FormSchema) { s.Version = "v2" }},
		{"frozen", func(p *SubmissionPlan, s *FormSchema) { s.Frozen = true }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			p, s := submissionFixture(t)
			tt.change(&p, &s)
			_, err := c.SubmitPlan(context.Background(), p, s)
			if err == nil {
				t.Fatal("invalid plan accepted")
			}
			if strings.Contains(err.Error(), syntheticToken()) {
				t.Fatal("session leaked in error")
			}
		})
	}
	if writes.Load() != 0 {
		t.Fatalf("writes=%d", writes.Load())
	}
}

func TestMissingRequiredFieldsReportedTogether(t *testing.T) {
	p, s := submissionFixture(t)
	delete(p.Payload, "title")
	delete(p.Payload, "trae_session_id")
	delete(p.Attachments, "x_trae_trace")
	err := ValidateSubmissionPlan(p, s)
	for _, key := range []string{"title", "trae_session_id", "x_trae_trace"} {
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("missing diagnostic for %s", key)
		}
	}
}

func TestConditionalRequiredField(t *testing.T) {
	p, s := submissionFixture(t)
	f := FormField{Key: "detail", Type: "text"}
	f.RequiredWhen.Field = "title"
	f.RequiredWhen.Values = []string{"task"}
	s.Fields = append(s.Fields, f)
	if err := ValidateSubmissionPlan(p, s); err == nil {
		t.Fatal("conditional requirement ignored")
	}
	p.Payload["detail"] = "explanation"
	if err := ValidateSubmissionPlan(p, s); err != nil {
		t.Fatal(err)
	}
}

func TestExistingSubmissionBlocksWrites(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		if r.URL.Path != "/submissions/1480" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"submission":{"id":1480,"status":"PENDING_FIX"},"locked_fields":["trae_session_id"],"values":{"trae_session_id":"未提供"}}}`))
	}))
	defer server.Close()
	c, _ := New(server.URL, nil)
	p, s := submissionFixture(t)
	p.ExistingSubmissionID = 1480
	_, err := c.SubmitPlan(context.Background(), p, s)
	if err == nil || !strings.Contains(err.Error(), `locks field "trae_session_id"`) {
		t.Fatalf("unexpected %v", err)
	}
	if writes.Load() != 0 {
		t.Fatalf("writes=%d", writes.Load())
	}
}

func TestSubmitPlanPreservesOriginalTokenAndExcludesMetadata(t *testing.T) {
	var uploads, creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/precheck/items/2409":
			_, _ = w.Write([]byte(`{"code":0,"data":{"status":"PRECHECK_PASSED","repo_url":"https://github.com/example/repo","requirement_prompt":"actual prompt"}}`))
		case "/uploads/attachment":
			uploads.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"url":"https://example.com/trace.md"}}`))
		case "/submissions/create":
			creates.Add(1)
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(r.FormValue("payload")), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["trae_session_id"] != syntheticToken() {
				t.Error("original token not preserved")
			}
			if payload["x_trae_trace"] != "https://example.com/trace.md" {
				t.Error("upload reference missing")
			}
			for _, key := range []string{"备注", "existing_submission_id", "execution_prompt", "attachments"} {
				if _, ok := payload[key]; ok {
					t.Errorf("internal field leaked: %s", key)
				}
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"id":1,"status":"PENDING"}}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, _ := New(server.URL, nil)
	c.SetSessionCookies("test-session", "test-csrf")
	p, s := submissionFixture(t)
	if _, err := c.SubmitPlan(context.Background(), p, s); err != nil {
		t.Fatal(err)
	}
	if uploads.Load() != 1 || creates.Load() != 1 {
		t.Fatalf("uploads=%d creates=%d", uploads.Load(), creates.Load())
	}
	if _, ok := p.Payload["x_trae_trace"]; ok {
		t.Fatal("caller payload mutated")
	}
}

func TestPrecheckMismatchBlocksUploads(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"status":"PRECHECK_PASSED","repo_url":"https://github.com/example/repo","requirement_prompt":"a different prompt"}}`))
	}))
	defer server.Close()
	c, _ := New(server.URL, nil)
	p, s := submissionFixture(t)
	if _, err := c.SubmitPlan(context.Background(), p, s); err == nil || !strings.Contains(err.Error(), "prompt differs") {
		t.Fatalf("unexpected %v", err)
	}
	if writes.Load() != 0 {
		t.Fatalf("writes=%d", writes.Load())
	}
}

func TestFormSchemaMatchesAcrossQuotaWindows(t *testing.T) {
	limited := FormSchema{Version: "v1", Limited: true, Fields: []FormField{{Key: "repo_url"}}}
	bound := FormSchemaSHA256(limited)
	reopened := limited
	reopened.Limited = false
	if !FormSchemaMatches(bound, reopened) {
		t.Fatal("reopened quota must keep the bound schema")
	}
	changed := reopened
	changed.Fields = append(changed.Fields, FormField{Key: "new_required"})
	if FormSchemaMatches(bound, changed) || FormSchemaMatches("", reopened) {
		t.Fatal("field rule changes or a missing binding must not match")
	}
}

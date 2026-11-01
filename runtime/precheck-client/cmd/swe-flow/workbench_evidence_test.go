package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	client "example.com/swe-precheck-client"
)

func TestClassifyReceiptFamilies(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{"qc", map[string]any{"server_verification": map[string]any{}, "new_submission_id": 2.0}, "submission_qc"},
		{"platform", map[string]any{"platform_status": "QC_RUNNING", "submission_id": 2.0}, "platform"},
		{"precheck", map[string]any{"batch_id": "b", "requirement_prompt_sha256": "h"}, "precheck"},
		{"trace", map[string]any{"session_sha256": "h", "user_message_count": 1.0}, "trace_export"},
		{"session", map[string]any{"session_token_sha256": "h"}, "session_rounds"},
		{"execution", map[string]any{"log_path": "/x", "execution_started_at": "t"}, "execution"},
		{"publication", map[string]any{"commit_url": "u", "branch": "b"}, "publication"},
		{"gate", map[string]any{"checks": []any{}, "submission": map[string]any{}}, "gate"},
		{"unknown", map[string]any{"hello": "world"}, "unknown"},
	}
	for _, test := range cases {
		if got := classifyReceipt(test.payload); got != test.want {
			t.Fatalf("%s: got %s want %s", test.name, got, test.want)
		}
	}
}

func TestReadJournalVerifiesAndDetectsTamper(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "sources", "demo")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "workflow", "demo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "task-state.json")
	ctx := context.Background()
	store := client.NewWorkflowStore(path)
	state, err := store.Init(ctx, "demo", workspace)
	if err != nil {
		t.Fatal(err)
	}
	state, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "test", Outcome: "success"}, func(next *client.WorkflowState) error {
		next.Status = client.WorkflowLocalOnly
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	events, chain, err := readJournal(path, state)
	if err != nil {
		t.Fatal(err)
	}
	if !chain.Verified || chain.Count != len(events) || chain.Count < 2 || !chain.HeadMatches {
		t.Fatalf("chain not verified: %+v", chain)
	}
	journal := filepath.Join(dir, "events.jsonl")
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0x01
	if err := os.WriteFile(journal, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, tampered, err := readJournal(path, state)
	if err != nil {
		t.Fatal(err)
	}
	if tampered.Verified {
		t.Fatal("tampered journal still verified")
	}
	if tampered.BrokenAt == 0 || tampered.Reason == "" {
		t.Fatalf("tamper not localized: %+v", tampered)
	}
}

func TestRubricEvidenceJoinsResults(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "workflow", "demo")
	if err := os.MkdirAll(filepath.Join(dir, "design"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "verification"), 0o700); err != nil {
		t.Fatal(err)
	}
	rubric := "rubrics:\n  - id: 1\n    type: f2p\n    text: 第一条\n  - id: 2\n    type: p2p\n    text: 第二条\n"
	if err := os.WriteFile(filepath.Join(dir, "design", "rubric.yaml"), []byte(rubric), 0o600); err != nil {
		t.Fatal(err)
	}
	results := `{"results":[{"id":1,"status":"PASS","reason":"ok"},{"id":2,"passed":false,"reason":"bad"}]}`
	if err := os.WriteFile(filepath.Join(dir, "verification", "rubric-results.json"), []byte(results), 0o600); err != nil {
		t.Fatal(err)
	}
	items, summary := rubricEvidence(filepath.Join(dir, "task-state.json"), client.WorkflowState{})
	if len(items) != 2 || summary.Passed != 1 || summary.Failed != 1 || summary.Total != 2 {
		t.Fatalf("unexpected rubric join: %+v %+v", items, summary)
	}
	if items[1].Conclusion != "FAIL" || items[1].Evidence != "bad" {
		t.Fatalf("second item not joined: %+v", items[1])
	}
}

func TestCollectReceiptsStaysInsideTask(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "workflow", "demo")
	if err := os.MkdirAll(filepath.Join(dir, "verification"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "workflow", "other", "receipt.json")
	if err := os.MkdirAll(filepath.Dir(outside), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "verification", "receipt.json"), outside} {
		if err := os.WriteFile(path, []byte(`{"status":"ok"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	items, err := collectReceipts(dir, client.WorkflowState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "receipt.json" {
		t.Fatalf("unexpected receipts: %+v", items)
	}
	if filepath.Dir(items[0].Path) != filepath.Join(dir, "verification") {
		t.Fatalf("receipt escaped task dir: %s", items[0].Path)
	}
}

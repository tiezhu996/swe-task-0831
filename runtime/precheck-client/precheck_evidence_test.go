package precheckclient

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rejectedItem() ItemDetail {
	return ItemDetail{
		BatchItem: BatchItem{ID: 9, BatchID: "b", RepoURL: "https://github.com/o/r", Status: "PRECHECK_REJECTED",
			RejectKind: "ISSUE_DUP", RejectLabel: "与 Issue 重复", RejectDetail: "命中 #22", Rerunnable: true,
			UpdatedAt: "2026-10-26T10:00:00Z", DifficultyTotal: 13},
		RequirementPrompt: "题面",
		IssueDup:          json.RawMessage(`[{"number":22}]`),
		LocalDup:          json.RawMessage(`null`),
	}
}

func TestSavePrecheckResultIsPrivateIdempotentAndRefusesChange(t *testing.T) {
	root := t.TempDir()
	path, err := SavePrecheckResult(root, rejectedItem(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "local_dup") || !strings.Contains(string(data), `"number": 22`) || strings.Contains(string(data), "题面") {
		t.Fatalf("unexpected content: %s", data)
	}
	if _, err := SavePrecheckResult(root, rejectedItem(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("identical re-read should be a no-op: %v", err)
	}
	changed := rejectedItem()
	changed.RejectDetail = "别的"
	if _, err := SavePrecheckResult(root, changed, time.Now()); err == nil {
		t.Fatal("changed verdict under same name was overwritten")
	}
}

func writeReceipt(t *testing.T, root, prompt string, gates map[string]int, inputs map[string]string) {
	t.Helper()
	list := []map[string]any{}
	for name, code := range gates {
		list = append(list, map[string]any{"name": name, "exit_code": code})
	}
	data, _ := json.Marshal(map[string]any{"schema_version": 1, "passed": true, "prompt_sha256": TextSHA256(CanonicalPrompt(prompt)), "gates": list, "inputs": inputs})
	if err := os.WriteFile(filepath.Join(root, PrecheckGateReceipt), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePrecheckGate(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "PROMPT.md")
	os.WriteFile(input, []byte("题面\n"), 0600)
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("题面\n")))
	all := map[string]int{"independence": 0, "feature_overlap": 0, "style": 0, "scope_depth": 0}

	if err := ValidatePrecheckGate(root, "题面"); err == nil {
		t.Fatal("missing receipt accepted")
	}
	writeReceipt(t, root, "题面", all, map[string]string{input: sum})
	if err := ValidatePrecheckGate(root, "题面\n"); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
	if err := ValidatePrecheckGate(root, "改过的题面"); err == nil {
		t.Fatal("receipt for another prompt accepted")
	}
	partial := map[string]int{"independence": 0, "feature_overlap": 1, "style": 0, "scope_depth": 0}
	writeReceipt(t, root, "题面", partial, map[string]string{input: sum})
	if err := ValidatePrecheckGate(root, "题面"); err == nil || !strings.Contains(err.Error(), "feature_overlap") {
		t.Fatalf("failed gate accepted: %v", err)
	}
	writeReceipt(t, root, "题面", all, map[string]string{input: sum})
	os.WriteFile(input, []byte("题面改了\n"), 0600)
	if err := ValidatePrecheckGate(root, "题面"); err == nil {
		t.Fatal("stale input accepted")
	}
}

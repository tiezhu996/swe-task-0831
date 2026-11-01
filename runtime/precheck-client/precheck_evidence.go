package precheckclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// PrecheckResultDir holds one file per platform verdict under the task root.
// precheck-status only kept the status before, so rejection reasons, the two
// duplicate reports and the difficulty scores were lost once the page changed.
const PrecheckResultDir = "precheck-results"

// PrecheckGateReceipt is written by scripts/precheck_gate.py after the four
// local gates. A new precheck or a repair is only sent for the prompt it binds.
const PrecheckGateReceipt = "precheck-gate.json"

// RequiredPrecheckGates are the local gates a receipt has to report as passed.
var RequiredPrecheckGates = []string{"independence", "feature_overlap", "style", "scope_depth"}

type PrecheckResult struct {
	SchemaVersion    int             `json:"schema_version"`
	FetchedAt        string          `json:"fetched_at"`
	ItemID           int             `json:"item_id"`
	BatchID          string          `json:"batch_id"`
	RepoURL          string          `json:"repo_url"`
	PromptSHA256     string          `json:"prompt_sha256"`
	Status           string          `json:"status"`
	StatusLabel      string          `json:"status_label"`
	UpdatedAt        string          `json:"updated_at"`
	Difficulty       string          `json:"difficulty"`
	DifficultyTotal  int             `json:"difficulty_total"`
	RejectKind       string          `json:"reject_kind"`
	RejectLabel      string          `json:"reject_label"`
	RejectDetail     string          `json:"reject_detail"`
	Rerunnable       bool            `json:"rerunnable"`
	Appealable       bool            `json:"appealable"`
	LocalDup         json.RawMessage `json:"local_dup,omitempty"`
	IssueDup         json.RawMessage `json:"issue_dup,omitempty"`
	DifficultyScores json.RawMessage `json:"difficulty_scores,omitempty"`
	Appeals          json.RawMessage `json:"appeals,omitempty"`
}

var unsafeName = regexp.MustCompile(`[^0-9A-Za-z]+`)

// TerminalPrecheckStatus reports whether the platform has reached a verdict.
func TerminalPrecheckStatus(status string) bool {
	return status == "PRECHECK_PASSED" || status == "PRECHECK_REJECTED" || status == "PRECHECK_FAILED"
}

// SavePrecheckResult stores one verdict as <root>/precheck-results/<id>-<updated_at>.json
// with mode 0600. Re-reading the same verdict is a no-op; a different verdict
// under the same name is refused rather than overwritten.
func SavePrecheckResult(root string, item ItemDetail, now time.Time) (string, error) {
	result := PrecheckResult{
		SchemaVersion: 1, FetchedAt: now.UTC().Format(time.RFC3339), ItemID: item.ID, BatchID: item.BatchID,
		RepoURL: item.RepoURL, PromptSHA256: TextSHA256(item.RequirementPrompt), Status: item.Status,
		StatusLabel: item.StatusLabel, UpdatedAt: item.UpdatedAt, Difficulty: item.Difficulty,
		DifficultyTotal: item.DifficultyTotal, RejectKind: item.RejectKind, RejectLabel: item.RejectLabel,
		RejectDetail: item.RejectDetail, Rerunnable: item.Rerunnable, Appealable: item.Appealable,
		LocalDup: nonNull(item.LocalDup), IssueDup: nonNull(item.IssueDup),
		DifficultyScores: nonNull(item.DifficultyScores), Appeals: nonNull(item.Appeals),
	}
	dir := filepath.Join(root, PrecheckResultDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("预检结果目录不可写")
	}
	stamp := unsafeName.ReplaceAllString(item.UpdatedAt, "")
	if stamp == "" {
		stamp = "unknown"
	}
	path := filepath.Join(dir, fmt.Sprintf("%d-%s.json", item.ID, stamp))
	if existing, err := os.ReadFile(path); err == nil {
		var old PrecheckResult
		if json.Unmarshal(existing, &old) == nil {
			old.FetchedAt = result.FetchedAt
			if samePrecheckResult(old, result) {
				return path, nil
			}
		}
		return "", fmt.Errorf("预检结果 %s 已存在且内容不同，未覆盖", filepath.Base(path))
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", fmt.Errorf("预检结果不可写")
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return "", fmt.Errorf("预检结果写入失败")
	}
	return path, nil
}

func samePrecheckResult(a, b PrecheckResult) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

func nonNull(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return raw
}

type precheckGateReceipt struct {
	SchemaVersion int    `json:"schema_version"`
	Passed        bool   `json:"passed"`
	PromptSHA256  string `json:"prompt_sha256"`
	Gates         []struct {
		Name     string `json:"name"`
		ExitCode int    `json:"exit_code"`
	} `json:"gates"`
	Inputs map[string]string `json:"inputs"`
}

// ValidatePrecheckGate checks the receipt in root against the prompt about to
// be sent: all four gates exited 0 for exactly this prompt, and none of the
// files the gates read has changed since.
func ValidatePrecheckGate(root, prompt string) error {
	var receipt precheckGateReceipt
	if err := ReadLocalJSON(filepath.Join(root, PrecheckGateReceipt), &receipt); err != nil {
		return fmt.Errorf("缺本地门禁回执，先运行 scripts/precheck_gate.py")
	}
	if receipt.SchemaVersion != 1 || !receipt.Passed {
		return fmt.Errorf("本地门禁未全部通过，不能发送预检")
	}
	if receipt.PromptSHA256 != TextSHA256(CanonicalPrompt(prompt)) {
		return fmt.Errorf("本地门禁回执不是这份题面的，题面改过后重新运行 scripts/precheck_gate.py")
	}
	passed := map[string]bool{}
	for _, gate := range receipt.Gates {
		if gate.ExitCode == 0 {
			passed[gate.Name] = true
		}
	}
	for _, name := range RequiredPrecheckGates {
		if !passed[name] {
			return fmt.Errorf("本地门禁 %s 未通过", name)
		}
	}
	if len(receipt.Inputs) == 0 {
		return fmt.Errorf("本地门禁回执缺输入文件哈希")
	}
	for path, want := range receipt.Inputs {
		data, err := os.ReadFile(path)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != want {
			return fmt.Errorf("门禁输入 %s 在回执之后改过，重新运行 scripts/precheck_gate.py", filepath.Base(path))
		}
	}
	return nil
}

package main

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	client "example.com/swe-precheck-client"
)

// ---------- trace ledger ----------

type traceLedgerView struct {
	Available         bool     `json:"available"`
	Path              string   `json:"path,omitempty"`
	Bytes             int64    `json:"bytes,omitempty"`
	SHA256            string   `json:"sha256,omitempty"`
	Lines             int      `json:"lines"`
	UserTurns         int      `json:"user_turns"`
	AssistantSections int      `json:"assistant_sections"`
	EffectiveRounds   int      `json:"effective_rounds"`
	OneHumanPrompt    bool     `json:"one_human_prompt"`
	Notes             []string `json:"notes,omitempty"`
	Error             string   `json:"error,omitempty"`
}

func traceLedger(taskPath string, state client.WorkflowState) traceLedgerView {
	view := traceLedgerView{EffectiveRounds: 0}
	if state.Trae.EffectiveRounds != nil {
		view.EffectiveRounds = *state.Trae.EffectiveRounds
	}
	tracePath := ""
	if artifact, ok := state.Verification.Artifacts["x_trae_trace"]; ok {
		tracePath = artifact.Path
	}
	if tracePath == "" {
		tracePath = filepath.Join(filepath.Dir(taskPath), "trajectories", "archive", "trae-export-sanitized.md")
	}
	if tracePath == "" || !pathContains(filepath.Dir(taskPath), tracePath) {
		view.Error = "轨迹路径不在任务目录内"
		return view
	}
	info, err := os.Lstat(tracePath)
	if err != nil || !info.Mode().IsRegular() {
		view.Error = "轨迹文件不存在"
		return view
	}
	view.Path, view.Bytes = tracePath, info.Size()
	if info.Size() > traceMaxBytes {
		view.Error = "轨迹超过读取上限"
		return view
	}
	data, err := os.ReadFile(tracePath)
	if err != nil {
		view.Error = "轨迹文件不可读"
		return view
	}
	view.SHA256 = hex.EncodeToString(sum256(data))
	if len(data) > 0 {
		view.Lines = bytes.Count(data, []byte("\n")) + 1
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.EqualFold(trimmed, "**User**") || strings.EqualFold(trimmed, "## User"):
			view.UserTurns++
		case strings.EqualFold(trimmed, "**Assistant**") || strings.EqualFold(trimmed, "## Assistant"):
			view.AssistantSections++
		}
	}
	view.Available = true
	view.OneHumanPrompt = view.UserTurns == 1
	if view.UserTurns > 1 {
		view.Notes = append(view.Notes, "轨迹里出现多条人工题面，违反单条题面约束")
	}
	if view.UserTurns == 0 {
		view.Notes = append(view.Notes, "未识别到人工题面标记，导出格式可能不同，结论记为 unknown")
	}
	if view.EffectiveRounds > 0 && view.AssistantSections == 1 {
		view.Notes = append(view.Notes, "有效轮数不能由 Markdown 分段推导；两者不一致不代表材料有错")
	}
	return view
}

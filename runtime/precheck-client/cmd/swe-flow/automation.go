package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	client "example.com/swe-precheck-client"
)

type nextAction struct {
	TaskID       string   `json:"task_id"`
	Status       string   `json:"status"`
	Revision     int64    `json:"revision"`
	Action       string   `json:"action"`
	Automatic    bool     `json:"automatic"`
	RequiresUser bool     `json:"requires_user"`
	Reason       string   `json:"reason"`
	Command      string   `json:"command,omitempty"`
	Missing      []string `json:"missing,omitempty"`
	Pending      bool     `json:"pending_operation"`
}

func actionForState(state client.WorkflowState, args map[string]string) nextAction {
	result := nextAction{TaskID: state.TaskID, Status: string(state.Status), Revision: state.Revision, Pending: state.PendingOperation != nil}
	if op := state.PendingOperation; op != nil && (op.Outcome == "started" || op.Outcome == "uncertain") {
		result.Action = "reconcile"
		result.RequiresUser = true
		result.Reason = "存在结果未知的外部写请求，必须先用平台真实回执核对，禁止自动重试"
		if op.Kind == "submit" {
			result.Command = "reconcile-submission"
		} else {
			result.Command = "reconcile-precheck"
		}
		return result
	}
	missing := func(keys ...string) bool {
		for _, key := range keys {
			if strings.TrimSpace(args[key]) == "" {
				result.Missing = append(result.Missing, key)
			}
		}
		return len(result.Missing) > 0
	}
	switch state.Status {
	case client.WorkflowInitialized:
		result.Action, result.Reason = "bind_project", "需要固定原始仓库、基线 commit 和语言"
		result.RequiresUser = true
	case client.WorkflowProjectSelected:
		if !state.Verification.Preflight.Passed {
			result.Action, result.Command = "preflight", "preflight"
			result.Reason = "统一预检尚未通过"
		} else {
			result.Action, result.Command = "preflight_or_submit_precheck", "precheck-submit"
			result.Reason = "题面尚未提交预检"
		}
		result.Automatic = !missing("prompt")
		result.RequiresUser = !result.Automatic
	case client.WorkflowPrecheckDraft, client.WorkflowPrecheckSubmitted:
		result.Action, result.Command = "check_precheck", "precheck-status"
		result.Reason = "读取平台预检状态并同步本地 state"
		result.Automatic = true
	case client.WorkflowPrecheckRejected:
		result.Action, result.Command = "repair_precheck", "precheck-repair"
		result.Reason = "预检被拒绝；先看 precheck-results 里的打回类型，按 references/PRECHECK_REJECTION.md 分类后由用户确认返修、重跑或申诉"
		result.RequiresUser = true
	case client.WorkflowPrecheckPassed:
		if state.PlanPath != "" && state.Trae.ExecutionMode == "manual" {
			result.Action, result.Reason = "collect_manual_evidence", "源码已准备；Trae 执行后补充 Session 与原生轨迹，随后交给 Codex 验收"
			result.RequiresUser = true
			return result
		}
		result.Action, result.Command = "prepare_plan", "prepare-plan"
		result.Reason = "预检已通过，准备绑定同一题面与 schema 快照"
		result.Automatic = !missing("template", "schema", "prompt", "out")
		result.RequiresUser = !result.Automatic
	case client.WorkflowTraeRunning:
		result.Action = "wait_for_trae"
		if state.Trae.ExecutionMode == "manual" {
			result.Action = "collect_manual_evidence"
			result.Reason = "Trae 执行后补充 Session 与原生轨迹，随后交给 Codex 验收"
			result.Missing = []string{"Session 原串", "原生 Markdown 轨迹"}
		} else {
			result.Reason = "需要真实 Trae UI 完成模型执行；自动化不能代替桌面真实性确认"
		}
		result.RequiresUser = true
	case client.WorkflowSessionCaptured:
		result.Action, result.Command = "verify_session", "session-verify"
		result.Reason = "从真实日志复核完整 Session 和有效轮数"
		result.Automatic = state.PlanPath != ""
		result.RequiresUser = !result.Automatic
	case client.WorkflowSessionVerified:
		result.Action, result.Command = "verify_artifacts", "artifact-check"
		result.Reason = "核对终端取证、Trae 导出、schema 和冻结材料"
		result.Automatic = !missing("terminal-evidence", "trace-receipt", "schema")
		result.RequiresUser = !result.Automatic
	case client.WorkflowArtifactsVerified:
		result.Action, result.Command = "dry_run", "dry-run"
		result.Reason = "执行提交前只读审核，不上传附件"
		result.Automatic = !missing("terminal-evidence", "trace-receipt", "schema")
		result.RequiresUser = !result.Automatic
	case client.WorkflowDryRunPassed:
		result.Action, result.Command = "submit", "submit"
		result.Reason = "审核已通过；正式上传和创建必须由显式 execute 授权"
		result.RequiresUser = true
	case client.WorkflowSubmitted, client.WorkflowQCPending, client.WorkflowQCRejected, client.WorkflowQCAccepted:
		result.Action, result.Command = "query_submission", "submission-status"
		result.Reason = "读取真实平台提交状态"
		result.Automatic = state.Submission.ID > 0
		result.RequiresUser = !result.Automatic
	case client.WorkflowClosed:
		result.Action, result.Reason = "closed", "任务已关闭"
	case client.WorkflowLocalOnly:
		result.Action, result.Reason = "local_only", "有效轮数不足 100，且评测为效果好或已完成；平台不收录，仅留存本地，可重新打开修改"
		result.RequiresUser = true
	default:
		result.Action, result.Reason = "inspect", "状态需要人工核对"
		result.RequiresUser = true
	}
	return result
}

func runNextAction(args []string) error {
	flags := flag.NewFlagSet("next-action", flag.ContinueOnError)
	statePath := flags.String("state", "task-state.json", "任务状态 JSON")
	prompt := flags.String("prompt", "", "题面路径")
	template := flags.String("template", "", "提交草稿模板路径")
	schema := flags.String("schema", "", "schema 快照路径")
	out := flags.String("out", "", "计划输出路径")
	terminal := flags.String("terminal-evidence", "", "原生终端取证清单")
	trace := flags.String("trace-receipt", "", "Trae 导出回执")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	state, err := client.NewWorkflowStore(*statePath).Load(ctx)
	if err != nil {
		return err
	}
	values := map[string]string{"prompt": *prompt, "template": *template, "schema": *schema, "out": *out, "terminal-evidence": *terminal, "trace-receipt": *trace}
	return printSummary(actionForState(state, values))
}

func runResume(args []string) error {
	flags := flag.NewFlagSet("resume", flag.ContinueOnError)
	statePath := flags.String("state", "task-state.json", "任务状态 JSON")
	prompt := flags.String("prompt", "", "题面路径")
	template := flags.String("template", "", "提交草稿模板路径")
	schema := flags.String("schema", "", "schema 快照路径")
	out := flags.String("out", "", "计划输出路径")
	terminal := flags.String("terminal-evidence", "", "原生终端取证清单")
	trace := flags.String("trace-receipt", "", "Trae 导出回执")
	tools := flags.String("tools", os.Getenv("SWE_FLOW_TOOLS"), "本地工具目录")
	preflightOut := flags.String("preflight-out", "", "预检报告路径")
	rubric := flags.String("rubric", "", "Rubric 文件路径")
	issues := flags.String("issues", "", "公开 Issue JSON 路径")
	execute := flags.Bool("execute", false, "显式允许平台写请求")
	untilBlocked := flags.Bool("until-blocked", false, "持续执行到人工阻断或完成")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("存在未识别的参数")
	}
	if !*untilBlocked {
		return fmt.Errorf("resume 需要 -until-blocked，避免无界自动循环")
	}
	values := map[string]string{"prompt": *prompt, "template": *template, "schema": *schema, "out": *out, "terminal-evidence": *terminal, "trace-receipt": *trace}
	for step := 0; step < 32; step++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		state, err := client.NewWorkflowStore(*statePath).Load(ctx)
		cancel()
		if err != nil {
			return err
		}
		next := actionForState(state, values)
		if next.RequiresUser || !next.Automatic || next.Action == "closed" {
			return printSummary(map[string]any{"stopped": true, "reason": next, "steps": step})
		}
		commandArgs := []string{"-state", *statePath}
		switch next.Command {
		case "precheck-submit":
			commandArgs = append(commandArgs, "-prompt", *prompt)
			if *execute {
				commandArgs = append(commandArgs, "-execute")
			} else {
				return printSummary(map[string]any{"stopped": true, "reason": "预检写请求需要显式 -execute", "next": next})
			}
		case "preflight":
			commandArgs = append(commandArgs, "-prompt", *prompt, "-tools", *tools)
			if *preflightOut != "" {
				commandArgs = append(commandArgs, "-out", *preflightOut)
			}
			if *rubric != "" {
				commandArgs = append(commandArgs, "-rubric", *rubric)
			}
			if *issues != "" {
				commandArgs = append(commandArgs, "-issues", *issues)
			}
		case "prepare-plan":
			commandArgs = append(commandArgs, "-plan", *template, "-schema", *schema, "-prompt", *prompt, "-out", *out)
		case "session-verify":
			commandArgs = append(commandArgs, "-tools", *tools)
		case "artifact-check", "dry-run":
			commandArgs = append(commandArgs, "-terminal-evidence", *terminal, "-trace-receipt", *trace, "-schema", *schema)
		case "submission-status":
		default:
			return printSummary(map[string]any{"stopped": true, "reason": next, "steps": step})
		}
		if next.Command == "preflight" {
			if err := runPreflight(commandArgs); err != nil {
				return err
			}
		} else if err := runOperation(next.Command, commandArgs); err != nil {
			return err
		}
		if next.Command == "prepare-plan" {
			if err := runLocal("bind-plan", []string{"-state", *statePath, "-plan", *out}); err != nil {
				return err
			}
		}
		if next.Command == "precheck-submit" && !*execute {
			return nil
		}
	}
	return fmt.Errorf("resume 达到 32 步上限，需重新读取 state")
}

func runPreflight(args []string) error {
	flags := flag.NewFlagSet("preflight", flag.ContinueOnError)
	statePath := flags.String("state", "task-state.json", "任务状态 JSON")
	prompt := flags.String("prompt", "", "冻结题面路径")
	out := flags.String("out", "", "预检报告输出路径")
	rubric := flags.String("rubric", "", "Rubric 文件路径")
	issues := flags.String("issues", "", "公开 Issue JSON 路径")
	profile := flags.String("profile", "normal", "预检 profile")
	tools := flags.String("tools", os.Getenv("SWE_FLOW_TOOLS"), "本地工具目录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *prompt == "" {
		return fmt.Errorf("preflight 需要 -prompt")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	store := client.NewWorkflowStore(*statePath)
	state, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = filepath.Join(filepath.Dir(*statePath), "preflight-report.json")
	}
	if *tools == "" {
		return fmt.Errorf("未找到预检工具；指定 -tools 或设置 SWE_FLOW_TOOLS")
	}
	command := []string{filepath.Join(*tools, "preflight.py"), "--prompt", *prompt, "--workspace", state.Workspace, "--profile", *profile, "--out", *out}
	if *rubric != "" {
		command = append(command, "--rubric", *rubric)
	}
	if *issues != "" {
		command = append(command, "--issues", *issues)
	}
	cmd := exec.CommandContext(ctx, "python3", command...)
	err = cmd.Run()
	reportBytes, readErr := os.ReadFile(*out)
	if readErr != nil {
		if err != nil {
			return fmt.Errorf("预检执行失败且没有报告: %w", err)
		}
		return fmt.Errorf("预检报告不可读")
	}
	var report struct {
		Passed       bool   `json:"passed"`
		Profile      string `json:"profile"`
		InputsSHA256 string `json:"inputs_sha256"`
	}
	if jsonErr := json.Unmarshal(reportBytes, &report); jsonErr != nil {
		return fmt.Errorf("预检报告格式无效")
	}
	absolute, _ := filepath.Abs(*out)
	state, updateErr := store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "preflight", Outcome: outcomeFor(err), PayloadSHA256: client.TextSHA256(string(reportBytes))}, func(s *client.WorkflowState) error {
		s.Verification.Preflight = client.WorkflowPreflight{Path: absolute, SHA256: client.TextSHA256(string(reportBytes)), InputsSHA256: report.InputsSHA256, Profile: report.Profile, Passed: report.Passed, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		return nil
	})
	if updateErr != nil {
		return updateErr
	}
	_ = state
	if err != nil || !report.Passed {
		return fmt.Errorf("统一预检未通过；报告已保存: %s", absolute)
	}
	return printSummary(map[string]any{"preflight": "passed", "path": absolute, "sha256": client.TextSHA256(string(reportBytes)), "revision": state.Revision})
}

func outcomeFor(err error) string {
	if err == nil {
		return "success"
	}
	return "failure"
}

func sha256File(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func runBatchStatus(args []string) error {
	flags := flag.NewFlagSet("batch-status", flag.ContinueOnError)
	root := flags.String("dir", ".", "包含多个任务目录的根目录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	var items []map[string]any
	counts := map[string]int{}
	err := filepath.WalkDir(*root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "task-state.json" || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		state, err := client.NewWorkflowStore(path).Load(context.Background())
		if err != nil {
			items = append(items, map[string]any{"path": path, "error": "state_unreadable"})
			counts["state_unreadable"]++
			return nil
		}
		next := actionForState(state, map[string]string{})
		items = append(items, map[string]any{"path": path, "task_id": state.TaskID, "status": state.Status, "revision": state.Revision, "next_action": next.Action, "requires_user": next.RequiresUser})
		counts[string(state.Status)]++
		return nil
	})
	if err != nil {
		return err
	}
	return printSummary(map[string]any{"root": *root, "count": len(items), "by_status": counts, "items": items})
}

func defaultPlanPath(state client.WorkflowState) string {
	if state.PlanPath != "" {
		return state.PlanPath
	}
	return filepath.Join(state.Workspace, "submission-plan.json")
}

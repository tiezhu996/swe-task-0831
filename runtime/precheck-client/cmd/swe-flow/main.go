package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	client "example.com/swe-precheck-client"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("用法：swe-flow <命令> -state <任务清单>；swe-flow help 查看命令")
	}
	command, args := os.Args[1], os.Args[2:]
	switch command {
	case "help", "--help", "-h":
		fmt.Print(`swe-flow <命令> -state <task-state.json>
本地：init status project-bind prepare-plan bind-plan import-history prompt-normalize
设计与字段：design-freeze -plan <设计草稿JSON>；field-review -out <报告JSON>
出题记录：precheck-design-save -prompt <题面> -plan <配套设计JSON> -language <语言>；precheck-design-bind -item-id <编号>
编排：next-action resume -until-blocked batch-status serve workbench-submit
表单规则：schema-fetch
预检：precheck-submit precheck-bind precheck-status precheck-repair precheck-rerun reconcile-precheck
执行记录：execution-mode -mode manual；trae-start -mode manual；session-verify
提交：artifact-check dry-run submit submission-status reconcile-submission reset-uncreated-submission
默认不执行平台写入；授权范围内加 -execute。trae-start 仅记录已确认的桌面操作。
`)
		return nil
	case "init", "status", "bind-plan", "advance", "import-history":
		return runLocal(command, args)
	case "next-action":
		return runNextAction(args)
	case "resume":
		return runResume(args)
	case "preflight":
		return runPreflight(args)
	case "batch-status":
		return runBatchStatus(args)
	case "serve":
		return runWorkbenchServe(args)
	case "workbench-submit":
		return runWorkbenchSubmit(args)
	default:
		return runOperation(command, args)
	}
}

func runLocal(command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	statePath := flags.String("state", "task-state.json", "任务流转清单 JSON")
	taskID := flags.String("task", "", "稳定任务标识")
	workspace := flags.String("workspace", ".", "项目工作区路径")
	planPath := flags.String("plan", "", "提交计划 JSON")
	receiptPath := flags.String("receipt", "", "历史服务端回执 JSON")
	to := flags.String("to", "", "下一状态")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("存在未识别的参数")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := client.NewWorkflowStore(*statePath)
	var state client.WorkflowState
	var err error
	if command == "init" {
		if *taskID == "" {
			return fmt.Errorf("init 需要 -task")
		}
		state, err = store.Init(ctx, *taskID, *workspace)
	} else if command == "import-history" {
		if *taskID == "" || *planPath == "" || *receiptPath == "" {
			return fmt.Errorf("import-history 需要 -task、-plan、-receipt")
		}
		state, err = store.ImportHistorical(ctx, *taskID, *workspace, *planPath, *receiptPath)
	} else {
		state, err = store.Load(ctx)
		if err != nil {
			return err
		}
		switch command {
		case "bind-plan":
			if *planPath == "" {
				return fmt.Errorf("bind-plan 需要 -plan")
			}
			state, err = store.BindPlan(ctx, state.Revision, *planPath)
		case "advance":
			if *to == "" {
				return fmt.Errorf("advance 需要 -to")
			}
			next := client.WorkflowStatus(*to)
			switch next {
			case client.WorkflowPrecheckPassed, client.WorkflowSessionVerified, client.WorkflowArtifactsVerified, client.WorkflowDryRunPassed, client.WorkflowSubmitted, client.WorkflowQCPending, client.WorkflowQCRejected, client.WorkflowQCAccepted:
				return fmt.Errorf("不能用 advance 人工声明平台或验证结论；请使用实际 API/日志操作")
			}
			state, err = store.Advance(ctx, state.Revision, next, client.WorkflowAudit{Action: "advance", Outcome: "success"})
		}
	}
	if err != nil {
		return err
	}
	// Status output has identities and hashes only; local paths stay on disk.
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"task_id": state.TaskID, "status": state.Status, "revision": state.Revision,
		"precheck_item_id": state.Precheck.ItemID, "precheck_status": state.Precheck.Status,
		"plan_sha256": state.PlanSHA256, "session_token_sha256": state.Trae.SessionTokenSHA256,
		"effective_rounds": state.Trae.EffectiveRounds, "submission_id": state.Submission.ID,
		"submission_status": state.Submission.Status, "pending_operation": state.PendingOperation,
	})
}

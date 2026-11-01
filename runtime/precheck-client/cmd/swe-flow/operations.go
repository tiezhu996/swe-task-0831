package main

import (
	"context"
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

func runOperation(command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	statePath := flags.String("state", "task-state.json", "任务状态 JSON")
	templatePath := flags.String("plan", "", "提交草稿模板 JSON")
	promptPath := flags.String("prompt", "", "草稿或预检题面文件")
	outPath := flags.String("out", "", "草稿输出文件")
	repo := flags.String("repo", "", "原始 GitHub 仓库")
	baseline := flags.String("baseline", "", "固定基线 commit")
	language := flags.String("language", "Go", "项目实际主要语言")
	model := flags.String("model", "Seed-Evolving", "Trae 默认模型")
	mode := flags.String("mode", "", "执行模式：manual（手动桌面执行）")
	schemaPath := flags.String("schema", "", "表单 schema 快照")
	sessionPath := flags.String("session-audit", "", "Session 核验 JSON")
	terminalPath := flags.String("terminal-evidence", "", "原生终端取证清单")
	tracePath := flags.String("trace-receipt", "", "Trae 应用导出回执")
	execute := flags.Bool("execute", false, "显式执行平台写请求")
	online := flags.Bool("online", false, "读取当前平台 schema 和预检")
	itemID := flags.Int("item-id", 0, "绑定真实预检编号")
	submissionID := flags.Int("submission-id", 0, "查询并恢复已有提交编号")
	tools := flags.String("tools", os.Getenv("SWE_FLOW_TOOLS"), "本地工具目录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("存在未识别的参数")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if command == "precheck-design-save" || command == "precheck-design-bind" {
		registry, err := client.DefaultPrecheckDesignStore()
		if err != nil {
			return err
		}
		if command == "precheck-design-save" {
			prompt, err := os.ReadFile(*promptPath)
			if err != nil {
				return fmt.Errorf("需要实际题面文件")
			}
			materials, err := client.ReadDesignMaterials(*templatePath)
			if err != nil {
				return fmt.Errorf("需要四项配套设计 JSON")
			}
			var schema client.FormSchema
			if *schemaPath != "" {
				if client.ReadLocalJSON(*schemaPath, &schema) != nil {
					return fmt.Errorf("schema 不可读")
				}
			} else {
				c, err := authenticatedClient(ctx)
				if err != nil {
					return err
				}
				schema, err = c.SubmissionFormSchema(ctx)
				if err != nil {
					return err
				}
			}
			record, err := registry.Save(ctx, client.CanonicalPrompt(string(prompt)), *language, materials, schema)
			if err != nil {
				return err
			}
			return printSummary(map[string]any{"design_recorded": true, "record_path": registry.RecordPath(record), "prompt_sha256": record.Materials.PromptSHA256, "network_writes": 0})
		}
		c, err := authenticatedClient(ctx)
		if err != nil {
			return err
		}
		item, err := c.ItemDetail(ctx, *itemID)
		if err != nil {
			return err
		}
		bound, err := registry.Bind(ctx, item)
		if err != nil {
			return err
		}
		if !bound {
			return fmt.Errorf("此预检没有对应的出题设计记录")
		}
		return printSummary(map[string]any{"design_bound": true, "precheck_item_id": item.ID, "network_writes": 0})
	}
	if command == "prompt-normalize" {
		data, err := os.ReadFile(*promptPath)
		if err != nil {
			return fmt.Errorf("题面文件不可读")
		}
		prompt := client.NormalizeDraftPrompt(string(data))
		if err := client.ValidateDraftPrompt(prompt); err != nil {
			return err
		}
		if *outPath == "" {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"draft_prompt": prompt, "characters": len([]rune(prompt)), "sha256": client.TextSHA256(prompt)})
		}
		if _, err := os.Stat(*outPath); !os.IsNotExist(err) {
			return fmt.Errorf("输出已存在，不覆盖已固定题面")
		}
		if err := os.WriteFile(*outPath, []byte(prompt), 0600); err != nil {
			return err
		}
		return printSummary(map[string]any{"draft_saved": true, "characters": len([]rune(prompt)), "sha256": client.TextSHA256(prompt)})
	}
	store := client.NewWorkflowStore(*statePath)
	state, err := store.Load(ctx)
	if err != nil {
		return err
	}
	update := func(action string, mutate func(*client.WorkflowState) error) error {
		var err error
		state, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: action, Outcome: "success"}, mutate)
		if err != nil {
			return err
		}
		return printState(state)
	}
	switch command {
	case "execution-mode":
		if *mode != "manual" || (state.Status != client.WorkflowPrecheckPassed && state.Status != client.WorkflowTraeRunning) || state.Submission.ID != 0 || state.PendingOperation != nil {
			return fmt.Errorf("只能为未提交的待采集任务登记手动模式")
		}
		if state.Trae.ExecutionMode == "manual" {
			return printState(state)
		}
		return update("manual_mode_selected", func(s *client.WorkflowState) error { s.Trae.ExecutionMode = "manual"; return nil })
	case "design-freeze":
		var materials client.DesignMaterials
		var schema client.FormSchema
		if client.ReadLocalJSON(*templatePath, &materials) != nil || client.ReadLocalJSON(state.Submission.SchemaPath, &schema) != nil {
			return fmt.Errorf("design-freeze 需要 -plan 指向设计草稿及已绑定 schema")
		}
		frozen, err := store.FreezeDesign(ctx, materials, schema)
		if err != nil {
			return err
		}
		return printState(frozen)
	case "field-review":
		plan, err := client.ReadSubmissionPlan(state.PlanPath)
		if err != nil {
			return err
		}
		var schema client.FormSchema
		if client.ReadLocalJSON(state.Submission.SchemaPath, &schema) != nil {
			return fmt.Errorf("缺少字段规则快照")
		}
		report := client.ReviewSubmissionFields(plan, schema)
		if *outPath != "" {
			if err := client.SaveLocalJSON(*outPath, report); err != nil {
				return err
			}
		}
		return printSummary(report)
	case "schema-fetch":
		if *outPath == "" {
			return fmt.Errorf("schema-fetch 需要 -out 保存当前表单规则")
		}
		if _, err := os.Lstat(*outPath); !os.IsNotExist(err) {
			return fmt.Errorf("schema 输出已存在或无法检查；使用新的快照文件")
		}
		c, err := authenticatedClient(ctx)
		if err != nil {
			return err
		}
		schema, err := c.SubmissionFormSchema(ctx)
		if err != nil || schema.Version == "" {
			return fmt.Errorf("平台 schema 读取失败")
		}
		if err := client.SaveLocalJSON(*outPath, schema); err != nil {
			return err
		}
		path, err := filepath.Abs(*outPath)
		if err != nil {
			return err
		}
		return update("schema_fetch", func(s *client.WorkflowState) error {
			s.Submission.SchemaPath = path
			s.Submission.SchemaSHA256 = client.FormSchemaSHA256(schema)
			return nil
		})
	case "prepare-plan":
		if *templatePath == "" || *schemaPath == "" || *promptPath == "" || *outPath == "" {
			return fmt.Errorf("prepare-plan 需要 -plan、-schema、-prompt、-out")
		}
		if _, err := os.Lstat(*outPath); !os.IsNotExist(err) {
			return fmt.Errorf("输出已存在或无法检查，不覆盖提交草稿")
		}
		plan, err := client.ReadSubmissionPlan(*templatePath)
		if err != nil {
			return err
		}
		schemaBytes, err := os.ReadFile(*schemaPath)
		if err != nil {
			return fmt.Errorf("schema 快照不可读")
		}
		var schema client.FormSchema
		if err := json.Unmarshal(schemaBytes, &schema); err != nil {
			return err
		}
		data, err := os.ReadFile(*promptPath)
		if err != nil {
			return fmt.Errorf("冻结题面不可读")
		}
		prompt := client.CanonicalPrompt(string(data))
		prepared, err := client.PrepareWorkflowPlan(state, plan, schema, prompt)
		if err != nil {
			return err
		}
		prepared.SchemaSHA256 = client.FormSchemaSHA256(schema)
		if state.Submission.SchemaSHA256 == "" || prepared.SchemaSHA256 != state.Submission.SchemaSHA256 {
			return fmt.Errorf("schema 快照未绑定到当前任务；先使用 schema-fetch 记录同一份快照")
		}
		data, err = json.MarshalIndent(prepared, "", "  ")
		if err != nil {
			return fmt.Errorf("提交草稿无法编码")
		}
		file, err := os.OpenFile(*outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("草稿保存失败，不覆盖已有文件")
		}
		_, writeErr := file.Write(append(data, '\n'))
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return fmt.Errorf("草稿保存不完整，需要检查本地文件")
		}
		return printSummary(map[string]any{"plan_prepared": true, "network_writes": 0, "precheck_item_id": state.Precheck.ItemID, "prompt_sha256": state.Precheck.PromptSHA256})
	case "reset-uncreated-submission":
		state, err := client.ResetUncreatedSubmission(ctx, store)
		if err != nil {
			return err
		}
		return printState(state)
	case "project-bind":
		if state.Status != client.WorkflowInitialized || !strings.HasPrefix(*repo, "https://github.com/") || *baseline == "" || strings.TrimSpace(*language) == "" {
			return fmt.Errorf("需要初始状态、原仓库和固定基线")
		}
		return update("project_bind", func(s *client.WorkflowState) error {
			s.Project = client.WorkflowProject{RepoURL: *repo, BaselineCommit: *baseline, Language: *language}
			s.Status = client.WorkflowProjectSelected
			return nil
		})
	case "model-set":
		// Record the user's model naming rule on an unsubmitted task. The
		// plan changes, so any later material audit must be redone.
		target := strings.TrimSpace(*model)
		if target == "" || state.Submission.ID != 0 || (state.PendingOperation != nil && (state.PendingOperation.Outcome == "started" || state.PendingOperation.Outcome == "uncertain")) || state.PlanPath == "" {
			return fmt.Errorf("只能为已有计划且未提交的任务修改模型名")
		}
		switch state.Status {
		case client.WorkflowTraeRunning, client.WorkflowSessionCaptured, client.WorkflowSessionVerified, client.WorkflowArtifactsVerified, client.WorkflowDryRunPassed:
		default:
			return fmt.Errorf("当前阶段不能修改模型名")
		}
		for _, back := range []client.WorkflowStatus{client.WorkflowArtifactsVerified, client.WorkflowSessionVerified} {
			if (state.Status == client.WorkflowDryRunPassed && back == client.WorkflowArtifactsVerified) || (state.Status == client.WorkflowArtifactsVerified && back == client.WorkflowSessionVerified) {
				if state, err = store.Advance(ctx, state.Revision, back, client.WorkflowAudit{Action: "model_set_reopen_review", Outcome: "success"}); err != nil {
					return err
				}
			}
		}
		if state.Trae.Model != target {
			if state, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "model_set", Outcome: "success"}, func(s *client.WorkflowState) error { s.Trae.Model = target; return nil }); err != nil {
				return err
			}
		}
		plan, err := client.ReadSubmissionPlan(state.PlanPath)
		if err != nil {
			return err
		}
		if plan.Payload["seed_model"] != target {
			plan.Payload["seed_model"] = target
			if err := client.SaveLocalJSON(state.PlanPath, plan); err != nil {
				return err
			}
		}
		if state, err = store.BindPlan(ctx, state.Revision, state.PlanPath); err != nil {
			return fmt.Errorf("模型名已写入，但计划绑定失败：%w", err)
		}
		return printState(state)
	case "trae-start":
		if state.Status != client.WorkflowPrecheckPassed || strings.TrimSpace(*model) == "" || strings.TrimSpace(*mode) != "manual" {
			return fmt.Errorf("只有预检通过、手动模式且确认实际模型后才能记录执行")
		}
		return update("trae_start", func(s *client.WorkflowState) error {
			s.Trae.ExecutionMode = "manual"
			s.Trae.ExecutionStartedAt = time.Now().UTC().Format(time.RFC3339Nano)
			s.Trae.Model = *model
			s.Status = client.WorkflowTraeRunning
			return nil
		})
	case "precheck-submit", "precheck-repair", "precheck-rerun":
		return precheckWrite(ctx, store, state, command, *promptPath, *execute)
	case "precheck-status", "precheck-bind":
		c, err := authenticatedClient(ctx)
		if err != nil {
			return err
		}
		if command == "precheck-bind" {
			if *itemID <= 0 || state.Status != client.WorkflowProjectSelected {
				return fmt.Errorf("绑定预检需要 project_selected 状态和真实编号")
			}
			data, err := os.ReadFile(*promptPath)
			if err != nil {
				return fmt.Errorf("绑定需要实际题面文件")
			}
			item, err := c.ItemDetail(ctx, *itemID)
			if err != nil {
				return fmt.Errorf("预检读取失败")
			}
			// PROMPT.md may have a final newline, but the platform prompt is literal.
			prompt := client.CanonicalPrompt(string(data))
			if item.ID != *itemID || item.BatchID == "" || item.RepoURL != state.Project.RepoURL || item.RequirementPrompt != prompt {
				return fmt.Errorf("预检仓库或原文不同，未绑定")
			}
			for _, next := range []client.WorkflowStatus{client.WorkflowPrecheckDraft, client.WorkflowPrecheckSubmitted} {
				state, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "precheck_bind", Outcome: "success", Method: "GET", Endpoint: fmt.Sprintf("/precheck/items/%d", *itemID)}, func(s *client.WorkflowState) error {
					s.Status = next
					s.Precheck = client.WorkflowPrecheck{ItemID: item.ID, BatchID: item.BatchID, PromptSHA256: client.TextSHA256(prompt)}
					return nil
				})
				if err != nil {
					return err
				}
			}
		}
		return syncPrecheck(ctx, c, store, state)
	case "reconcile-precheck":
		if state.PendingOperation == nil || !strings.HasPrefix(state.PendingOperation.Kind, "precheck") || state.Status != client.WorkflowPrecheckDraft || *itemID <= 0 {
			return fmt.Errorf("需要未决预检写请求和真实预检编号")
		}
		c, err := authenticatedClient(ctx)
		if err != nil {
			return err
		}
		item, err := c.ItemDetail(ctx, *itemID)
		if err != nil {
			return fmt.Errorf("预检读取失败")
		}
		if item.ID != *itemID {
			return fmt.Errorf("预检读取身份不匹配；写请求仍未决")
		}
		if err := client.ValidatePrecheckRecovery(state, item); err != nil {
			return fmt.Errorf("缺少新批次或条目的可核验记录；写请求仍未决，禁止重试")
		}
		state, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "reconcile", Method: "GET", Endpoint: fmt.Sprintf("/precheck/items/%d", item.ID), OperationID: state.PendingOperation.ID, Outcome: "reconciled", ResultID: item.ID}, func(s *client.WorkflowState) error {
			s.PendingOperation.Outcome = "reconciled"
			s.Precheck = client.WorkflowPrecheck{ItemID: item.ID, BatchID: item.BatchID, Status: item.Status, PromptSHA256: client.TextSHA256(item.RequirementPrompt)}
			s.Status = client.WorkflowPrecheckSubmitted
			return nil
		})
		if err != nil {
			return err
		}
		return syncPrecheck(ctx, c, store, state)
	case "session-verify", "artifact-check", "dry-run", "submit":
		if state.PlanPath == "" {
			return fmt.Errorf("需要先 bind-plan")
		}
		if *sessionPath == "" {
			*sessionPath = filepath.Join(filepath.Dir(state.PlanPath), "submission-session-audit.json")
		}
		if command == "session-verify" || command == "submit" {
			if *tools == "" {
				executable, _ := os.Executable()
				for _, candidate := range []string{filepath.Join(filepath.Dir(executable), "..", "tools"), "tools", "../tools"} {
					if info, err := os.Stat(filepath.Join(candidate, "capture_trae_session.py")); err == nil && info.Mode().IsRegular() {
						*tools = candidate
						break
					}
				}
				if *tools == "" {
					return fmt.Errorf("未找到 Session 工具；指定 -tools 或设置 SWE_FLOW_TOOLS")
				}
			}
			if filepath.Clean(*sessionPath) != filepath.Clean(filepath.Join(filepath.Dir(state.PlanPath), "submission-session-audit.json")) {
				return fmt.Errorf("实时核验使用 plan 同目录的 Session 回执；不能读取另一个缓存副本")
			}
			arguments := []string{filepath.Join(*tools, "capture_trae_session.py"), "--plan", state.PlanPath, "--workspace", state.Workspace, "--from-plan", "--apply", "--task-state", *statePath}
			if historicalSessionAudit(*sessionPath) {
				// A historical capture has no live renderer log left to replay;
				// its evidence is the per-session snapshot repository, so the
				// re-check has to read the same source.
				arguments = append(arguments, "--historical")
			}
			cmd := exec.CommandContext(ctx, "python3", arguments...)
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("真实 Session 日志复核失败，未上传")
			}
		}
		plan, err := client.ReadSubmissionPlan(state.PlanPath)
		if err != nil {
			return err
		}
		var e client.AuditEvidence
		e.Workspace = state.Workspace
		if err := client.ReadLocalJSON(*sessionPath, &e.Session); err != nil {
			return fmt.Errorf("缺 Session 核验回执")
		}
		if command == "session-verify" {
			if state.Status != client.WorkflowTraeRunning && state.Status != client.WorkflowSessionCaptured {
				return fmt.Errorf("Session 核验阶段不正确")
			}
			if state.Status == client.WorkflowTraeRunning {
				state, err = store.Advance(ctx, state.Revision, client.WorkflowSessionCaptured, client.WorkflowAudit{Action: "session_captured", Outcome: "success"})
				if err != nil {
					return err
				}
			}
			data, err := os.ReadFile(*sessionPath)
			if err != nil {
				return err
			}
			return update("session_verified", func(s *client.WorkflowState) error {
				token, _ := plan.Payload["trae_session_id"].(string)
				payloadData, _ := json.Marshal(plan.Payload)
				var payload map[string]any
				_ = json.Unmarshal(payloadData, &payload)
				rounds, roundsOK := payload["effective_rounds"].(float64)
				// A historical receipt is backed by Trae's per-session snapshot
				// repository instead of live renderer logs, so it legitimately
				// carries no matched log; every other binding must still agree.
				historicalReceipt := e.Session.SessionSource == "trae_session_snapshot" || e.Session.Source == "historical_session_snapshot"
				if e.Session.TokenSHA256 != client.TextSHA256(token) || !e.Session.Applied || !e.Session.WorkspaceVerified || e.Session.WorkspaceSHA256 != client.TextSHA256(s.Workspace) || (e.Session.MatchingLogCount <= 0 && !historicalReceipt) || !roundsOK || rounds != float64(e.Session.EffectiveRoundCount) || s.Trae.EffectiveRounds == nil || *s.Trae.EffectiveRounds != e.Session.EffectiveRoundCount {
					return fmt.Errorf("Session 证据不匹配")
				}
				s.Trae.SessionTokenSHA256 = e.Session.TokenSHA256
				s.Trae.SessionAuditPath = *sessionPath
				s.Trae.SessionAuditSHA256 = client.TextSHA256(string(data))
				s.Status = client.WorkflowSessionVerified
				return nil
			})
		}
		if *terminalPath == "" || *tracePath == "" {
			return fmt.Errorf("需要 -terminal-evidence 和 -trace-receipt")
		}
		if err := client.ReadLocalJSON(*terminalPath, &e.Terminal); err != nil {
			return err
		}
		e.TerminalDirectory = filepath.Dir(*terminalPath)
		if err := client.ReadLocalJSON(*tracePath, &e.Trace); err != nil {
			return err
		}
		var schema client.FormSchema
		var c *client.Client
		if *online || (command == "submit" && *execute) {
			c, err = authenticatedClient(ctx)
			if err != nil {
				return err
			}
			schema, err = c.SubmissionFormSchema(ctx)
			if err != nil {
				return fmt.Errorf("schema 读取失败")
			}
		} else {
			if *schemaPath == "" {
				return fmt.Errorf("本地审核需要 -schema")
			}
			if err := client.ReadLocalJSON(*schemaPath, &schema); err != nil {
				return err
			}
		}
		if state.Submission.SchemaSHA256 != "" || plan.SchemaSHA256 != "" {
			if state.Submission.SchemaSHA256 == "" || plan.SchemaSHA256 == "" || state.Submission.SchemaSHA256 != plan.SchemaSHA256 || !client.FormSchemaMatches(state.Submission.SchemaSHA256, schema) {
				return fmt.Errorf("schema 快照已漂移；重新 schema-fetch 并重新生成 plan")
			}
		}
		audit := client.AuditSubmission(plan, schema, e)
		for _, field := range schema.Fields {
			if field.Key != "verify_rubric" {
				continue
			}
			if err := client.VerifyDesignBinding(state, plan); err != nil {
				audit.Checks["frozen_design"] = client.AuditCheck{Code: "missing_or_changed_design"}
				audit.Passed = false
			} else {
				audit.Checks["frozen_design"] = client.AuditCheck{Passed: true}
			}
			if err := client.VerifySemanticFields(state, plan, schema); err != nil {
				audit.Checks["field_semantic_review"] = client.AuditCheck{Code: "missing_or_changed_field_evidence"}
				audit.Passed = false
			} else {
				audit.Checks["field_semantic_review"] = client.AuditCheck{Passed: true}
			}
			break
		}
		if state.Submission.ID > 0 {
			audit.Passed = false
			audit.Checks["create_unique"] = client.AuditCheck{Code: "existing_submission"}
		}
		if c != nil {
			if err := c.AuditPrecheck(ctx, plan, &audit); err != nil {
				return err
			}
		}
		reportPath := filepath.Join(filepath.Dir(*statePath), "submission-audit.json")
		if err := client.SaveLocalJSON(reportPath, audit); err != nil {
			return err
		}
		if !audit.Passed {
			_ = printSummary(audit)
			return fmt.Errorf("提交审核未通过：%s；无上传、无创建", strings.Join(client.AuditFailures(audit), ", "))
		}
		if command == "submit" {
			if !*execute {
				return printSummary(audit)
			}
			result, err := c.SubmitWorkflow(ctx, store, plan, schema, e)
			if err != nil {
				return err
			}
			return printSummary(result)
		}
		if command == "artifact-check" {
			if state.Status != client.WorkflowSessionVerified {
				return fmt.Errorf("材料审核阶段不正确")
			}
			return update("artifacts_verified", func(s *client.WorkflowState) error { s.Status = client.WorkflowArtifactsVerified; return nil })
		}
		if state.Status != client.WorkflowArtifactsVerified && state.Status != client.WorkflowDryRunPassed {
			return fmt.Errorf("需先完成 artifact-check")
		}
		data, _ := os.ReadFile(reportPath)
		return update("dry_run_passed", func(s *client.WorkflowState) error {
			s.Submission.AuditedPlanSHA256 = audit.PlanSHA256
			s.Submission.AuditPath = reportPath
			s.Submission.AuditSHA256 = client.TextSHA256(string(data))
			s.Status = client.WorkflowDryRunPassed
			return nil
		})
	case "submission-status", "reconcile-submission":
		c, err := authenticatedClient(ctx)
		if err != nil {
			return err
		}
		id := state.Submission.ID
		if command == "reconcile-submission" {
			id = *submissionID
		}
		if id <= 0 {
			return fmt.Errorf("需要真实提交编号")
		}
		detail, err := c.SubmissionDetail(ctx, id)
		if err != nil {
			return fmt.Errorf("提交读取失败")
		}
		if detail.Submission.ID != id {
			return fmt.Errorf("提交身份不匹配")
		}
		if state.PlanPath == "" {
			return fmt.Errorf("缺提交计划")
		}
		plan, err := client.ReadSubmissionPlan(state.PlanPath)
		if err != nil {
			return err
		}
		if detail.Values["trae_session_id"] != plan.Payload["trae_session_id"] || detail.Values["repo_url"] != plan.Payload["repo_url"] {
			return fmt.Errorf("提交不属于本任务")
		}
		if client.TextSHA256(plan.ExecutionPrompt) != state.Precheck.PromptSHA256 || fmt.Sprint(detail.Values["precheck_item_id"]) != fmt.Sprint(plan.Payload["precheck_item_id"]) {
			return fmt.Errorf("服务端预检绑定与任务不同")
		}
		if command == "reconcile-submission" && (state.PendingOperation == nil || state.PendingOperation.Kind != "submit" || state.Status != client.WorkflowSubmitting) {
			return fmt.Errorf("没有待核对提交写请求")
		}
		action := "submission_status"
		opID := ""
		if command == "reconcile-submission" {
			action = "reconcile"
			opID = state.PendingOperation.ID
		}
		state, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: action, Outcome: "reconciled", Method: "GET", Endpoint: fmt.Sprintf("/submissions/%d", id), ResultID: id, OperationID: opID}, func(s *client.WorkflowState) error {
			s.Submission.ID = id
			s.Submission.Status = detail.Submission.Status
			if command == "reconcile-submission" {
				s.PendingOperation.Outcome = "reconciled"
				s.Status = client.WorkflowSubmitted
			}
			if command == "submission-status" {
				switch detail.Submission.Status {
				case "PENDING_QC", "QC_RUNNING":
					s.Status = client.WorkflowQCPending
				case "PENDING_FIX", "REJECTED":
					s.Status = client.WorkflowQCRejected
				case "QC_PASSED", "PASSED", "ACCEPTED":
					s.Status = client.WorkflowQCAccepted
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		return printState(state)
	default:
		return fmt.Errorf("未知命令：%s", command)
	}
}

func authenticatedClient(ctx context.Context) (*client.Client, error) {
	c, err := client.New("", nil)
	if err != nil {
		return nil, err
	}
	credentials, err := localCredentials()
	if err != nil {
		return nil, err
	}
	session, csrf := credentials.Session, credentials.CSRF
	if session != "" && csrf != "" {
		c.SetSessionCookies(session, csrf)
		user, err := c.Me(ctx)
		if err == nil && (credentials.Username == "" || user.Username == credentials.Username) {
			return c, nil
		}
		// An expired cached session falls through to the stored password instead
		// of making the account unusable until the file is edited by hand.
	}
	username, password := credentials.Username, credentials.Password
	if username == "" || password == "" {
		return nil, fmt.Errorf("本地 Session 失效且没有可用的密码；请在设置中重新登录该账号")
	}
	user, err := c.Login(ctx, username, password)
	if err != nil || user.Username != username {
		return nil, fmt.Errorf("登录失败，不输出凭据")
	}
	if session, csrf, ok := c.SessionCookies(); ok {
		updateSWEAccountSession(user.Username, session, csrf)
	}
	return c, nil
}

func printSummary(value any) error { return json.NewEncoder(os.Stdout).Encode(value) }
func printState(s client.WorkflowState) error {
	return printSummary(map[string]any{"task_id": s.TaskID, "status": s.Status, "revision": s.Revision, "precheck_item_id": s.Precheck.ItemID, "submission_id": s.Submission.ID, "platform_status": s.Submission.Status, "pending_operation": s.PendingOperation})
}

func precheckWrite(ctx context.Context, store *client.WorkflowStore, state client.WorkflowState, command, promptPath string, execute bool) error {
	if state.Status != client.WorkflowProjectSelected && state.Status != client.WorkflowPrecheckDraft && state.Status != client.WorkflowPrecheckRejected {
		return fmt.Errorf("当前阶段禁止重新预检")
	}
	if state.PendingOperation != nil && (state.PendingOperation.Outcome == "started" || state.PendingOperation.Outcome == "uncertain") {
		return fmt.Errorf("存在结果未知的写请求，先核对平台")
	}
	prompt := ""
	if command == "precheck-rerun" {
		if state.Precheck.ItemID <= 0 {
			return fmt.Errorf("缺预检编号")
		}
	} else {
		data, err := os.ReadFile(promptPath)
		if err != nil {
			return fmt.Errorf("题面文件不可读")
		}
		prompt = client.CanonicalPrompt(string(data))
		if err := client.ValidateDraftPrompt(prompt); err != nil {
			return err
		}
		// 新建和返修都只发本地四道门禁通过的那份题面；原样重跑不改题面，不需要回执。
		if err := client.ValidatePrecheckGate(filepath.Dir(store.Path), prompt); err != nil {
			return err
		}
	}
	if state.Project.RepoURL == "" {
		return fmt.Errorf("未固定原始仓库")
	}
	if !execute {
		return printSummary(map[string]any{"dry_run": true, "network_writes": 0, "prompt_sha256": client.TextSHA256(prompt), "precheck_item_id": state.Precheck.ItemID})
	}
	c, err := authenticatedClient(ctx)
	if err != nil {
		return err
	}
	var before *client.WorkflowPrecheck
	if command == "precheck-submit" || command == "precheck-repair" {
		if err := recordDraftForPrecheck(ctx, state, prompt, filepath.Dir(store.Path), c); err != nil {
			return err
		}
	}
	var priorIDs []int
	if command != "precheck-submit" {
		item, err := c.ItemDetail(ctx, state.Precheck.ItemID)
		if err != nil {
			return fmt.Errorf("预检读取失败")
		}
		if !item.Rerunnable {
			return fmt.Errorf("平台不允许重跑或返修")
		}
		if item.ID != state.Precheck.ItemID || item.RepoURL != state.Project.RepoURL || client.TextSHA256(item.RequirementPrompt) != state.Precheck.PromptSHA256 {
			return fmt.Errorf("预检身份、仓库或题面绑定已变化，未写入")
		}
		if command == "precheck-rerun" {
			prompt = item.RequirementPrompt
		}
		before = &client.WorkflowPrecheck{ItemID: item.ID, BatchID: item.BatchID, Status: item.Status, PromptSHA256: client.TextSHA256(item.RequirementPrompt)}
	} else {
		if state.Precheck.ItemID > 0 || state.Precheck.BatchID != "" {
			return fmt.Errorf("已绑定预检，不重复新建")
		}
		quota, err := c.Quota(ctx)
		if err != nil || quota.Allowance < 1 {
			return fmt.Errorf("预检配额不足或读取失败")
		}
		items, err := c.ListAllItems(ctx, client.ItemQuery{})
		if err != nil {
			return fmt.Errorf("无法保存预检写入前快照，未提交")
		}
		for _, item := range items {
			priorIDs = append(priorIDs, item.ID)
			if item.RepoURL == state.Project.RepoURL {
				detail, err := c.ItemDetail(ctx, item.ID)
				if err != nil || detail.ID != item.ID {
					return fmt.Errorf("历史预检核对失败，未提交")
				}
				if detail.RequirementPrompt == prompt {
					return fmt.Errorf("同仓库题面已存在于预检 %d；使用 precheck-bind 绑定该记录", item.ID)
				}
			}
		}
	}
	if state.Status != client.WorkflowPrecheckDraft {
		state, err = store.Advance(ctx, state.Revision, client.WorkflowPrecheckDraft, client.WorkflowAudit{Action: "precheck_draft", Outcome: "success"})
		if err != nil {
			return err
		}
	}
	rows := []client.Row{{RepoURL: state.Project.RepoURL, Prompt: prompt}}
	hash, _ := client.RowsSHA256(rows)
	endpoint := "/precheck/batches"
	if command != "precheck-submit" {
		endpoint = fmt.Sprintf("/precheck/items/%d", state.Precheck.ItemID)
		if command == "precheck-rerun" {
			endpoint += "/rerun"
		}
	}
	kind := "precheck_submit"
	if command == "precheck-repair" {
		kind = "precheck_repair"
	} else if command == "precheck-rerun" {
		kind = "precheck_rerun"
	}
	state, err = client.BeginPrecheckWrite(ctx, store, state, kind, endpoint, hash, before, priorIDs)
	if err != nil {
		return err
	}
	var batchID string
	itemID := state.Precheck.ItemID
	if command == "precheck-submit" {
		result, writeErr := c.SubmitBatch(ctx, rows)
		err = writeErr
		batchID = result.BatchID
		if err == nil && (batchID == "" || result.Accepted != 1) {
			err = fmt.Errorf("预检未确认接收")
		}
	} else {
		var result client.BatchItem
		if command == "precheck-repair" {
			result, err = c.UpdateItem(ctx, itemID, state.Project.RepoURL, prompt)
		} else {
			result, err = c.Rerun(ctx, itemID)
		}
		batchID = result.BatchID
		if result.ID > 0 {
			itemID = result.ID
		}
		if err == nil && (result.ID <= 0 || batchID == "" || (result.RepoURL != "" && result.RepoURL != state.Project.RepoURL)) {
			err = fmt.Errorf("平台未返回有效预检回执")
		}
	}
	if err != nil {
		recovery, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = client.FinishWorkflowWrite(recovery, store, state, endpoint, 0, false, nil)
		return fmt.Errorf("预检写入结果未知，不能自动重试；先查询平台")
	}
	state, err = client.FinishWorkflowWrite(ctx, store, state, endpoint, itemID, true, func(s *client.WorkflowState) error {
		s.Precheck = client.WorkflowPrecheck{BatchID: batchID, ItemID: itemID, PromptSHA256: client.TextSHA256(prompt), Status: "PENDING"}
		s.Status = client.WorkflowPrecheckSubmitted
		return nil
	})
	if err != nil {
		return fmt.Errorf("预检已接收但本地保存失败，先恢复日志并核对平台")
	}
	return printState(state)
}

func syncPrecheck(ctx context.Context, c *client.Client, store *client.WorkflowStore, state client.WorkflowState) error {
	id := state.Precheck.ItemID
	if id == 0 {
		progress, err := c.BatchProgress(ctx, state.Precheck.BatchID)
		if err != nil {
			return fmt.Errorf("预检批次读取失败")
		}
		if len(progress.Items) != 1 {
			return fmt.Errorf("批次条目不唯一，未自动绑定")
		}
		id = progress.Items[0].ID
	}
	item, err := c.ItemDetail(ctx, id)
	if err != nil {
		return fmt.Errorf("预检条目读取失败")
	}
	if item.ID != id || (state.Precheck.BatchID != "" && item.BatchID != state.Precheck.BatchID) || item.RepoURL != state.Project.RepoURL || client.TextSHA256(item.RequirementPrompt) != state.Precheck.PromptSHA256 {
		return fmt.Errorf("平台题面或仓库发生变化，未同步")
	}
	resultPath := ""
	if client.TerminalPrecheckStatus(item.Status) {
		resultPath, err = client.SavePrecheckResult(filepath.Dir(store.Path), item, time.Now())
		if err != nil {
			return err
		}
	}
	state, err = store.Update(ctx, state.Revision, client.WorkflowAudit{Action: "precheck_status", Method: "GET", Endpoint: fmt.Sprintf("/precheck/items/%d", id), Outcome: "success", ResultID: id}, func(s *client.WorkflowState) error {
		s.Precheck.ItemID = id
		s.Precheck.BatchID = item.BatchID
		s.Precheck.Status = item.Status
		if s.Status == client.WorkflowPrecheckSubmitted {
			if item.Status == "PRECHECK_PASSED" {
				s.Status = client.WorkflowPrecheckPassed
			} else if item.Status == "PRECHECK_REJECTED" || item.Status == "PRECHECK_FAILED" {
				s.Status = client.WorkflowPrecheckRejected
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if item.Status == "PRECHECK_PASSED" {
		registry, err := client.DefaultPrecheckDesignStore()
		if err != nil {
			return err
		}
		if _, err := registry.Bind(ctx, item); err != nil {
			return fmt.Errorf("预检状态已保存，但配套设计绑定需核查: %w", err)
		}
	}
	if item.Status == "PRECHECK_REJECTED" || item.Status == "PRECHECK_FAILED" {
		// 打回时直接给出类型和处理入口，详情在 precheck-results 里。
		return printSummary(map[string]any{"task_id": state.TaskID, "status": state.Status, "revision": state.Revision, "precheck_item_id": item.ID,
			"platform_status": item.Status, "reject_kind": item.RejectKind, "reject_label": item.RejectLabel, "reject_detail": item.RejectDetail,
			"difficulty_total": item.DifficultyTotal, "rerunnable": item.Rerunnable, "appealable": item.Appealable,
			"result_file": resultPath, "next": "按 references/PRECHECK_REJECTION.md 分类处理"})
	}
	return printState(state)
}

// historicalSessionAudit reports whether a saved Session receipt came from
// Trae's per-session snapshot repository instead of a live renderer log. A
// historical task has no log left to replay, so its re-check must read the
// same durable source rather than fail on missing logs.
func historicalSessionAudit(path string) bool {
	var audit struct {
		Source        string `json:"source"`
		SessionSource string `json:"session_source"`
	}
	if client.ReadLocalJSON(path, &audit) != nil {
		return false
	}
	return audit.Source == "historical_session_snapshot" || audit.SessionSource == "trae_session_snapshot"
}

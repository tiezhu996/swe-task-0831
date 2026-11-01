package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	client "example.com/swe-precheck-client"
)

// Collecting evidence after a manual run must retain the original preparation
// receipt. Model changes are expected here; only the baseline binding is checked.
func (w *workbenchServer) prepareManualCapture(ctx context.Context, path string, state client.WorkflowState) (client.WorkflowState, error) {
	if state.Trae.ExecutionMode != "" && state.Trae.ExecutionMode != "manual" {
		return state, fmt.Errorf("当前执行模式不能接收手动材料")
	}
	prompt, err := os.ReadFile(filepath.Join(filepath.Dir(path), "PROMPT.md"))
	if err != nil || client.TextSHA256(client.CanonicalPrompt(string(prompt))) != state.Precheck.PromptSHA256 {
		return state, fmt.Errorf("冻结题面与预检绑定不一致，未采集材料")
	}
	receiptPath := filepath.Join(filepath.Dir(path), "source-preparation.json")
	if _, err := os.Stat(receiptPath); err == nil {
		var receipt sourcePreparation
		if client.ReadLocalJSON(receiptPath, &receipt) != nil || receipt.SchemaVersion != 1 || receipt.Workspace != state.Workspace || receipt.RepoURL != state.Project.RepoURL || receipt.BaselineCommit != state.Project.BaselineCommit || receipt.PromptSHA256 != state.Precheck.PromptSHA256 || receipt.PrecheckID != state.Precheck.ItemID {
			return state, fmt.Errorf("原始源码准备回执与本题不一致")
		}
	} else if !os.IsNotExist(err) {
		return state, fmt.Errorf("原始源码准备回执不可读")
	}
	if exec.CommandContext(ctx, "git", "-C", state.Workspace, "merge-base", "--is-ancestor", state.Project.BaselineCommit, "HEAD").Run() != nil {
		return state, fmt.Errorf("本题固定基线不在当前源码历史中；请核对工作区")
	}
	if state.PlanPath == "" {
		if err := w.prepareTaskPlan(ctx, path, state); err != nil {
			return state, err
		}
		state, err = client.NewWorkflowStore(path).Load(ctx)
		if err != nil {
			return state, err
		}
	}
	// Older workbenches prepared the plan before recording the model. Carry
	// that existing declaration forward without overwriting conflicting data.
	if state.Trae.Model != "" {
		plan, err := client.ReadSubmissionPlan(state.PlanPath)
		if err != nil {
			return state, fmt.Errorf("提交计划不可读")
		}
		if current := plan.Payload["seed_model"]; current != state.Trae.Model {
			if current != nil && current != "" {
				return state, fmt.Errorf("模型记录与提交计划冲突，需按原始证据核对")
			}
			if !samePlanHash(state) {
				return state, fmt.Errorf("提交计划已变化，请核对绑定后采集")
			}
			var schema client.FormSchema
			if client.ReadLocalJSON(state.Submission.SchemaPath, &schema) != nil {
				return state, fmt.Errorf("字段规则快照不可读，无法同步旧模型记录")
			}
			prepared, err := client.PrepareWorkflowPlan(state, plan, schema, client.CanonicalPrompt(string(prompt)))
			if err != nil {
				return state, fmt.Errorf("旧模型记录不符合本题字段规则")
			}
			if err := client.SaveLocalJSON(state.PlanPath, prepared); err != nil {
				return state, fmt.Errorf("同步旧模型记录失败")
			}
			state, err = client.NewWorkflowStore(path).BindPlan(ctx, state.Revision, state.PlanPath)
			if err != nil {
				return state, fmt.Errorf("模型已同步，但计划绑定需核对")
			}
		}
	}
	if state.Status == client.WorkflowPrecheckPassed || state.Trae.ExecutionMode == "" {
		return client.NewWorkflowStore(path).Update(ctx, state.Revision, client.WorkflowAudit{Action: "manual_evidence_collection", Outcome: "success"}, func(s *client.WorkflowState) error {
			s.Trae.ExecutionMode = "manual"
			if s.Status == client.WorkflowPrecheckPassed {
				s.Status = client.WorkflowTraeRunning
			}
			// This is the collection time, not a fabricated execution start time.
			return nil
		})
	}
	return state, nil
}

func manualCodexHandoff(path string, state client.WorkflowState) string {
	return manualCodexHandoffMode(path, state, false)
}

// manualCodexHandoffMode chooses whether Codex stops at a local, reviewed
// state or goes on to the platform write once every gate passes.
func manualCodexHandoffMode(path string, state client.WorkflowState, submit bool) string {
	root := filepath.Dir(path)
	intro := fmt.Sprintf("按 swe-task 继续处理手动执行后的 SWE 任务 %s（预检 #%d）。我已在 Trae 输入冻结题面并执行完毕，现在需要后续验收和提交。\n\n原仓库：%s\n固定基线：%s\n模型源码工作区：%s\n任务状态：%s\n冻结题面文件：%s\n题面 SHA-256：%s\n当前记录阶段：%s\n", workbenchProjectName(state.Precheck.ItemID, state.Project.RepoURL, state.TaskID), state.Precheck.ItemID, state.Project.RepoURL, state.Project.BaselineCommit, state.Workspace, path, filepath.Join(root, "PROMPT.md"), state.Precheck.PromptSHA256, state.Status)
	missing := []string{}
	if state.Trae.SessionTokenSHA256 == "" {
		missing = append(missing, "完整 Trae Session 原串")
	}
	if _, ok := state.Verification.Artifacts["x_trae_trace"]; !ok {
		missing = append(missing, "该会话的 Trae 原生 Markdown 轨迹")
	}
	if len(missing) > 0 {
		intro += "用户待补材料：" + strings.Join(missing, "、") + "，仅在本地工作台补充；不要要求在聊天或命令参数中粘贴 Session 原串。\n"
	} else {
		intro += "Session 与轨迹已登记，复核现有材料即可，不要求用户重复提供。\n"
	}
	if state.Trae.SessionVerificationSkipped {
		intro += "注意：工作台本次按用户要求跳过了 Session 日志核验，只保存了原串哈希；请按原始日志与轨迹复核，不要把“已登记”当成“已核验”。\n"
	}
	if state.Verification.DesignPath != "" {
		intro += fmt.Sprintf("本题已有冻结设计：%s。先读取其中四项材料和研究依据，核对绑定后复用；不要重新推理已记录的任务类型、真实性与难度、模块和 Rubric。\n", state.Verification.DesignPath)
	}
	intro += "\n先读取当前 state、plan 和源码准备回执，核查已有执行材料。沿用本题源码改动，源码已经变动是执行产物，不能再要求点击开始或恢复成干净基线；禁止重新输入题面、继续旧模型会话、重跑任务或代改模型源码。执行开始/结束时间、模型版本和轮数从真实日志与轨迹核实，不能用准备时间或材料上传时间代替。\n\nSeed 模型/版本字段（seed_model）没有特别要求时统一填写 Seed-Evolving，不追加 Max、版本号或其他后缀；已有任务不一致时用 swe-flow model-set -state <本题state> -model Seed-Evolving 修改。\n\n用户只负责补充 Session 和原生轨迹。你负责复核原文、完整日志、唯一人工输入和工具读取路径，核实实际模型及有效轮数；冻结当前模型产物，在模型目录外按已固定 Rubric 逐项运行验收，生成真实终端截图及报告，发布仅含本题改动的任务提交，回填其余平台字段并执行材料审核和 dry-run。字段默认锁定是防误编辑，不代表已经填写或核验通过；缺项应由你据实际证据补齐，不能猜值。证据缺口须明确记录，不能补造执行前证据或宣称通过。\n\n" + handoffSubmitRule(submit) + ""
	intro += fmt.Sprintf("\n\n设计材料统一放在 %s，Rubric 为 rubric.yaml，四项材料为 materials.json。历史预检缺少 task_type、authenticity_note、involved_modules、verify_rubric 时，先只依据原始固定基线和冻结题面研究补齐，Rubric 按 swe-task 技能“Rubric 覆盖面”逐句拆题面、按维度自查后再冻结，不能根据模型缺陷反推验收标准；标明 historical_reconstruction，保存研究依据与文件哈希，在评价产物前用 design-freeze -state <本题state> -plan <设计草稿JSON> 冻结。已有冻结材料直接复用，不能再改 Rubric。\n\n提交前运行 field-review，修复逐字段格式、枚举、Rubric 编号、逐项结果、Type 与轮数、完成状态及提交链接的合规问题；自动规则不能证明语义。保存 field-review-evidence.json，schema_version=1，plan_sha256 使用 field-review 输出的 plan_sha256，prompt_sha256 为本题原文哈希，evidence 按 review_required 字段逐项记录 conclusion 与 sources（文件 path、sha256、bytes）。必须实际阅读原始源码、日志、Rubric 和结果证据后填写，缺证据不能声明通过。\n\n本题原生 Markdown 可放入 %s 后在网页点击检测并选用，也可上传；archive 中是私有原件、脱敏轨迹及回执。", filepath.Join(root, "design"), filepath.Join(root, "trajectories"))
	return intro
}

func handoffSubmitRule(submit bool) string {
	if submit {
		return "本次选择【审核通过后直接提交到 SWE 平台】：先同样写入 workbench-review.json（schema_version=1；plan_sha256、prompt_sha256、trace_sha256 取 GET /api/tasks/<任务key>/handoff 返回的 review_template；四个复核布尔值只在实际核对后为 true；notes 写明核对的输入、轨迹位置、源码差异和逐项验收证据），再用 swe-flow submit -state <本题state> -terminal-evidence <manifest> -trace-receipt <receipt> -execute 提交，并用 submission-status 查询平台真实回执。平台返回提交额度已满（submission_limited）或暂停提交（submit_frozen）时不要重试、不要改材料，保留审核记录让任务停在“本地已完成·待提交”，并报告平台原因。已有提交编号只查询、不重复创建，未决写请求先 reconcile。只处理本题，不启动下一题，不操作用户的 Trae 会话或用屏幕监控重新执行本题。"
	}
	return "本次选择【留在本地，不提交】：你只负责审核到 dry-run 通过，然后在任务目录写入 workbench-review.json（schema_version=1；plan_sha256、prompt_sha256、trace_sha256 取 GET /api/tasks/<任务key>/handoff 返回的 review_template；session_log_reviewed、one_human_prompt、source_diff_reviewed、rubric_evidence_reviewed 只有实际核对后才填 true；notes 写明核对的输入、轨迹位置、源码差异和逐项验收证据）。写入后停止，任务会在工作台显示为“本地已完成·待提交”；不要运行 submit -execute 或 workbench-submit，正式提交由用户之后在工作台点击。已有提交编号只查询、不重复创建，未决写请求先 reconcile。只处理本题，不启动下一题，不操作用户的 Trae 会话或用屏幕监控重新执行本题。"
}

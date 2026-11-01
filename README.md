# swe-task

这是一套用于研究与发布 SWE-like 开发题的 Codex 技能。它覆盖常规 `normal` 任务和历史验收复用的 `test` 任务：前者输出题面、验收条目和交付表，后者输出 `tests` 测试包及对应答辩文档。

当前版本：`1.0.0`。完整版本号以根目录 [VERSION](VERSION) 为唯一来源。

## 目录

- `SKILL.md`：技能的主说明和执行规范。
- `scripts/build_delivery_xlsx.py`：生成并校验 24 列交付表。
- `scripts/check_prompt_independence.py`：检查题面、难度说明和验收条目是否复用了公开 Issue 内容。
- `scripts/check_prompt_rubric_scope.py`：检查 Prompt 条款与 Rubric 的映射、新颖性证据、出题深度（`depth`：必改位置、停止陷阱、预测轮数）和越界要求。
- `scripts/check_prompt_style.py`：检查题面的顿号清单、规格腔、固定段落结构和代码标识符。
- `references/ROUND_DEPTH_DESIGN.md`：历史轮数与轨迹度量、改动处与停止陷阱、题面写法。
- `scripts/trajectory_metrics.py`：从 Trae 原始日志统计工具调用和改-跑-改循环，并私有归档会话日志。
- `scripts/screen_project.py`：在固定基线上筛项目，含断网容器测试；只有 PASS 才出题。
- `scripts/check_issue_feature_overlap.py`：抓全部 Issue/PR，校验逐条分类的功能重合台账。
- `references/PROJECT_SELECTION.md`：两步走流程、项目门禁、回测与功能重合台账格式。
- `references/DESIGN_PROMPT_TEMPLATE.md`：交给模型执行第一步出题的标准提示词。
- `scripts/precheck_gate.py`：一次跑完四道本地门禁并写预检回执，swe-flow 发送前校验。
- `scripts/trial_run.py`：便宜模型试跑（二次确认预演、完整试跑换算 TraeCN 轮数、历史校准、预检门禁检查）。
- `references/PRECHECK_REJECTION.md`：预检被打回后的取证、分类处理和记录。
- `scripts/count_trae_interactions.py`：按统一口径统计 Trae 会话有效轮数。
- `scripts/publish_task_commits.py`：把已完成任务发布到 GitHub fork 的 `taskN` 分支。
- `references/TEST_DEFENSE_TEMPLATE.md`：`test` 任务答辩文档模板。
- `references/TEST_SUBMISSION_REVIEW.md`：testzip 提交前复审流程与放行规则。
- `scripts/review_test_bundle.py`：检查 testzip 与 staging 一致性、文件安全、配置、语法、固定基线补丁、`test.patch` 是否被入口消费、未公开契约字面量绑定、JSON 字段别名、错误 vague/panic 负向探针、无 git 回退、`test.patch`/deliverytests 同步，以及输入耦合、零命中/多类型/pretty JSON 正例、stub/merge/generic-error/target-echo/tempfile-write 负向探针、TMPDIR/HOME/cwd 只读覆盖、既有测试回归映射、缓存/构建产物和公平性/导入导出证据。
- `references/TEST_PRE_SUBMISSION_REVIEW_TEMPLATE.md`：记录真实运行证据、Rubric 映射、复审人和 READY/HOLD/REWORK 结论。
- `tests/`：技能脚本的自动化测试。

## 安装

```bash
git clone https://github.com/tiezhu996/swe-task-0831.git \
  ~/.codex/skills/swe-task
python3 -m pip install -r requirements.txt
bash scripts/build_runtime.sh
```

需要运行测试时，安装开发依赖：

```bash
python3 -m pip install -r requirements-dev.txt
```

## 依赖

- 运行依赖：`openpyxl>=3.1,<4`
- 测试依赖：`pytest>=8,<9`

Git、Go、GitHub CLI（`gh`）和 Python 3 是执行发布流程所需的外部工具。

## 验证

```bash
cd ~/.codex/skills/swe-task
python3 -m pytest -q
```

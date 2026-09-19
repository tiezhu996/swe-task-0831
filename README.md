# swe-task-0831

这是一套用于研究与发布 SWE-like 开发题的 Codex 技能。它覆盖项目筛选、题面设计、验收条目编写、公开 Issue 独立性检查、Trae 轮次统计，以及题目变更到 GitHub fork 分支的发布流程。

## 目录

- `SKILL.md`：技能的主说明和执行规范。
- `scripts/build_delivery_xlsx.py`：生成并校验 24 列交付表。
- `scripts/check_prompt_independence.py`：检查题面、难度说明和验收条目是否复用了公开 Issue 内容。
- `scripts/count_trae_interactions.py`：按统一口径统计 Trae 会话有效轮数。
- `scripts/publish_task_commits.py`：把已完成任务发布到 GitHub fork 的 `taskN` 分支。
- `tests/`：技能脚本的自动化测试。

## 安装

```bash
git clone https://github.com/tiezhu996/swe-task-0831.git \
  ~/.codex/skills/swe-task-0831
```

## 验证

```bash
cd ~/.codex/skills/swe-task-0831
python3 -m pytest -q
```

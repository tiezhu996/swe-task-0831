# 选项目与功能重合门禁

出题分两步走，第一步全部通过后才进入第二步：

1. **出题与预检**：找项目 → `screen_project.py` 通过 → 抓 Issue/PR 语料 → 新项目先出 1 道探针题，试跑没被熔断、TraeCN 真实轮数落在窗口内才继续 → 在基线上设计需求 → 本地四道静态门禁（文本独立、功能不重合、文风、范围与深度）→ 便宜模型试跑落在窗口内（第五道门禁 `trial`）→ 平台预检通过。
2. **正式执行**：只有预检状态为通过的冻结题面才进入 Trae。任何一道门禁没过，回到第一步换需求或换项目，不能带着问题进第二步再补。

一个项目出 8-10 道题，每批 3-4 道，批批接着走第一步预检。项目筛查和 Issue 功能索引按项目做一次；语料每批开始前重新抓；功能重合台账每道题一份，既比 Issue/PR，也比同项目的其他题。

用过的项目不再选（2026-10-31 用户决定）：`data/used-projects.json` 记着所有出过题的项目。每开新的一批（用户说“重新开始”或开下一批 10 道）先运行 `python3 scripts/used_projects.py sync`，把 workflow 和 swe-archive 里出现过的项目全部记进表，批次中途不运行。表里的项目永不再用；本批选定、workflow 里已有任务但还没进表的项目可以继续出满 10 道，沿用第一次的筛查报告，不重筛。`find_swe_projects.py` 自动剔除表里和 workflow 里已有任务的项目，`screen_project.py` 的 `used` 门禁兜底；平台上做过但本地没有记录的项目用 `used_projects.py add --repo owner/name --reason ...` 手动补。

本文件管第一步里的两件事：项目能不能用，需求有没有和已有 Issue/PR 或同项目的其他题撞车。

## 1. 项目筛查

```bash
python scripts/screen_project.py \
  --repo owner/name --source /绝对路径/sources/<编号>-<项目> --commit <基线 commit> \
  --language Go --out-dir <项目目录>/screen-<基线短哈希> --run-container
```

不加 `--run-container` 只出 `INCOMPLETE`，不能据此出题。退出码 0 只对应 `PASS`。报告 `project-screen.json` 不覆盖，重跑换目录。每个项目在固定基线上筛一次，8-10 道题共用这份报告；同项目所有题用同一个基线。

| 门禁 | 默认阈值 | 为什么 |
|---|---|---|
| issues | 不设上限，只记录数量 | 数量就是功能索引要写多少条；Issue 多的项目前期工作量大，但不影响能不能用 |
| maturity | 非测试源码 ≥ 100 个文件、测试 ≥ 20 个、基线时仓库 ≥ 365 天、180 天内有推送、未归档非 fork | 小仓库的需求只有一两个接入点，凑不出 12 处改动 |
| license / language | 有明确 SPDX 许可证；GitHub 主语言等于 `--language` | 交付表要求 |
| external_deps | 硬件、媒体、推理运行时关键字单项 < 20 处 | 模型在本机跑不通就写说明收尾（3370 只有 11 轮） |
| history | 同项目已执行 ≥ 2 题且过 100 轮的不到一半 → 失败；全没过 → 警告；已有 ≥ 10 题 → 警告 | 项目本身的天花板；一个项目出 8-10 道题，再多新需求很难避开旧题 |
| used | 排除表 `data/used-projects.json` 或 workflow 里已有这个项目的任务 → 失败 | 用过的项目不再选；本批已选定的项目沿用第一次的报告，不重筛 |
| container | 联网构建后 `--network none` 跑全量测试；全过为 pass，有可定位的既有失败为 warn | 唯一能证明“方便验证”的门禁 |

`--waive-external TERM=理由` 只用于确认外部运行时在可选路径上、验收碰不到的情况。

### 关于“已经 Docker 化”

所有候选项目都带 Dockerfile（marmot 只在 examples/ 下有），所以有没有 Dockerfile 区分不了好坏。项目的 Dockerfile 一般是打发布镜像用的，不跑测试。筛查改用官方语言镜像在容器里构建和断网测试；项目自带根目录 Dockerfile、compose 文件和 compose 里的依赖服务（redis、postgres、minio 等）记在 `preferences` 里，同等条件下优先。

### 构建命令以项目 CI 为准

报告的 `preferences.ci_test_commands` 列出项目 CI 实际跑的命令。Go 项目的默认命令会自动加 `go generate`（有 `//go:generate` 时），但构建标签、环境变量要按 CI 自己传：

- 634 Gokapi 基线 0142d4c 不先 `go generate` 会因缺 `web/e2e.wasm` 构建失败；测试要带 `test` 标签。`--go-tags test,awsmock` 后 46 个包断网全过。
- 非 Go 项目必须传 `--image`、`--prepare-cmd`、`--test-cmd`。

照 CI 也构建不过，项目就不能用：3380 的基线 `db` 包编译不过，模型直接以“预存问题”收尾。

报告里的 `baseline_health` 块原样抄进 `design-contract.json` 的 `depth.baseline_health`。

### 校准与取舍

2026-10-26 用历史项目回测（“好项目”= 已执行的题过 100 轮的过半）：

| 项目 | 历史 | 筛查结果 | 判断 |
|---|---|---|---|
| ezbookkeeping | 3/3 | 原 300 条上限下 FAIL（517 条），上限取消后不再拦 | 对 |
| marmot | 4/5 | 元数据全过（容器未跑） | 对 |
| Gokapi | 1/1 | PASS（含容器） | 对 |
| hubproxy | 1/5 | FAIL：非测试源码 11 个 | 对 |
| chinese-poetry-api | 0/3 | FAIL：源码 51 个、基线时 302 天 | 对 |
| castor | 0/2 | FAIL：245 天；ffmpeg、whisper、dlna 等 | 对 |

要说清楚的几点：

- history 门禁用的就是这些历史结果，对回测没有独立证明力。真正有区分力的是 maturity 和 external_deps，单靠这两项 6 个项目全部分对。
- 原先的 300 条 Issue 上限误杀了最好的项目；这批样本里 Issue 少的反而是小而新的仓库（chinese-poetry 19 条、castor 24 条）。2026-10-26 取消上限，改由第 2 节保证不重合：Issue 再多也要逐条写进功能索引。
- 阈值来自 6 个项目，是暂定的。新项目执行完后在 [ROUND_DEPTH_DESIGN.md](ROUND_DEPTH_DESIGN.md) 第 1 节补行，攒够样本再调阈值。

### 探针题：用试跑给项目定档（2026-10-30 起）

筛查只有下限（源码 ≥100 个文件、测试 ≥20 个），能拦住太小的项目，拦不住太大的：需求落在一大块子系统上、全量测试要跑很久、或者每个功能都要前后端一起改的项目，TraeCN 一跑就是两三百轮。项目大不大，最终要看模型在它上面实际干多少活，所以新项目在筛查 PASS 之后、出整批题之前，先出 **1 道探针题**：

1. 按正常流程设计这一道题，过四道静态门禁。
2. 用 `trial_run.py confirm` 和 `trial_run.py run --runs 2` 试跑（见 [ROUND_DEPTH_DESIGN.md](ROUND_DEPTH_DESIGN.md) 第 6 节）。
3. 结果写进项目目录的 `probe.json`：题名、题面哈希、试跑判定、换算轮数、两次试跑的工具调用数和时长。
4. 试跑被熔断拦下（改动源码文件中位数 ≥14 或工具调用超限）就先缩小需求再探一次。没被拦，就照常预检、发 TraeCN 执行，真实轮数写进 `probe.json`：落在 110-200 就用这个项目出满 8-10 道，探针题本身也算一道；不到 100 换个改动更分散的需求再探一次；超过 200 缩小需求再探一次。**同一项目连续两道探针题落在同一侧，就换项目**，不要硬改需求去迁就项目。
5. 为什么要上 TraeCN 才能定档：10-30 的校准表明，便宜模型的试跑量换算到 TraeCN 轮数，跨项目误差约 30 轮，两三百轮的大题能靠“铺开”拦住，过不过 100 轮判断不了（见 ROUND_DEPTH_DESIGN.md 第 6 节校准结果）。探针题执行完，后面同项目的题可以拿试跑指标和它比大小。

候选项目的顺序：先看 `projects/` 下已经有 `probe.json` 且判定 `in_window`、题数还没到 10 道的项目；没有再从新候选里挑。新候选之间，下面几条只用来排先后，不是门禁（还没有足够样本验证）：

- 需求能落在一两个包里、但要碰多处调用点的业务项目优先，比如 ggshield、Gokapi 这类单语言、模块边界清楚的项目；
- 每个功能都必须前后端一起改的大型全栈项目、全量测试断网跑超过 10 分钟的项目、主要代码是生成出来的项目，往后排；
- 筛查报告里 `container` 一项的测试耗时也看一眼，测试慢会拉长每次改-跑-改，轮数跟着涨。

## 2. 功能重合

`check_prompt_independence.py` 只能抓到抄了措辞的情况，换个说法就过了，英文 Issue 和中文题面之间更是比不出来。这道门禁比的是功能，比两类对象：项目的全部 Issue/PR，和同项目的其他题。

项目级文件放在 `<项目目录>` = `~/repositories/标注任务/swe/projects/<项目名>/`，所有题共用；不要放进 `workflow/`，那里只放 `<编号>-<项目名>` 任务目录。台账放在各题自己的任务目录。都在模型工作区之外。

**每个项目一次：功能索引。** `issue-features.json` 给每条 Issue 写一句它要什么，8-10 道题共用，不用每题重读一遍原文：

```json
{"features": {"22": "私有仓库上游的认证配置", "55": "把被限流的 IP 存成文件"}}
```

**每批开始前：重新抓语料，补索引。**

```bash
python scripts/check_issue_feature_overlap.py fetch --repo owner/name --out <项目目录>/issues-<日期>.json
python scripts/check_issue_feature_overlap.py index --corpus <项目目录>/issues-<日期>.json --features <项目目录>/issue-features.json
```

`index` 列出新增、还没写进索引的 Issue，补完才往下走。语料也可以作为 `check_prompt_independence.py --issues` 的输入。

**每道题：台账。** 先生成骨架，Issue 的 feature 从索引带出来，命中检索词的 PR 也会列进去：

```bash
python scripts/check_issue_feature_overlap.py skeleton --corpus <语料> --features <索引> \
  --search-term quota --search-term bandwidth --search-term usage \
  --search-term 额度 --search-term 流量 --search-term 按人 --out <任务目录>/overlap-ledger.json
```

然后补全：`feature` 一句话写本题新增什么，`prompt_sha256` 填题面的 SHA-256，每个条目填 `relation`，需要时填 `reason`；`siblings` 给同项目的每道其他题写一条：

```json
{"name": "3375-hubproxy", "prompt_sha256": "<那道题 PROMPT.md 的 sha256>", "feature": "上游故障时切换，恢复后切回",
 "relation": "adjacent", "reason": "那道题改上游选择和健康检查；本题改计量与身份，不改上游选择"}
```

```bash
python scripts/check_issue_feature_overlap.py check --repo owner/name --corpus <语料> --features <索引> \
  --prompt <任务目录>/PROMPT.md --ledger <任务目录>/overlap-ledger.json \
  --sibling 同批题B=<草稿路径> --sibling 同批题C=<草稿路径>
```

同项目的其他题由脚本自动找：workflow 根目录下 `task-state.json` 里写着同一仓库、带 `PROMPT.md` 的任务，不管有没有执行、有没有通过，都要比。同一批里还没建任务目录的草稿用 `--sibling` 逐道传。

规则：

- 每条 Issue 必须分类一次，不能重复，也不能有语料里不存在的编号。PR 只在命中检索词时必须分类；没合并的 PR 一样算，因为它就是一份公开的实现。
- `relation`：`unrelated` 无关，`adjacent` 相邻（同一块代码或同一类用户，要的东西不同），`overlap` 重合（对方要的能力是本题要求的一部分，或者本题要求的是对方的一部分）。
- 有一条 `overlap` 就失败，不管是 Issue、PR 还是同项目的题。只能换掉重合的那部分需求，改写措辞不算修复。同批两道题重合时，留一道，另一道换。
- `adjacent`、命中检索词但标 `unrelated` 的 Issue/PR，以及所有同项目题，`reason` 至少 15 个字，写清分界在哪。同项目的题共用一份代码，标“无关”也要说为什么不碰同一块状态。
- 检索词至少 6 个，其中英文至少 3 个；要包含同义说法（quota/limit/usage、login/auth/token）。命中数在 `term_hits` 里，全是 0 的词换掉。
- 语料重新抓取、本题题面改动、同项目某道题题面改动，对应的哈希都会对不上，要重新核对。

### A+B 判重，不因附加 C 降级

2026-10-27 的 RDFLib 5127 返修给出实际判例：公开 Issue #225 的 A 是“ConjunctiveGraph 聚合时命名图/context 的归属数据”，B 是“把另一个图合入当前图的聚合阶段”。新题额外要求来源报告、预览、原子提交和幂等，但这些只是 C；A+B 已完全相同，平台仍判 `duplicate`。

因此命中相关 Issue/PR 后，先读正文中的核心对象和触发阶段，再决定关系：

- A 相同且 B 相同：判 `overlap`，即使用户体验、输出格式、失败恢复或原子性明显更丰富；
- A 相同但 B 不同：最多 `adjacent`，写清两个触发阶段为何不同；
- A 不同：才可按 B 的相似度判断，不能只因为都在同一模块就声称无关。

`issue-features.json` 只写标题时，只能作为检索索引，不能作为判重结论。凡是 `hit_terms` 命中的条目，尤其是 graph merge、import、copy、aggregation、transaction、context 这类近义词，必须回读正文。写 `unrelated` 前先回答“核心数据对象是否相同”和“生命周期阶段是否相同”两个问题。

例子：hubproxy 配额题里“拉镜像前先登录”命中了一个未合并的 PR（Web 登录 + 拉镜像 JWT 认证，840 行），属于 `overlap`；按月扣流量、命中缓存也计量这一半是新的。按规则要把登录换成别的身份来源，或者换题。它和 3374（访问来源和流量统计）是 `adjacent`，要写清本题是按人限额，那道题只是统计。

## 3. 第一步完成的标准

进入平台预检前应该有：

- 项目级：`project-screen.json`（verdict 为 PASS）及两份容器日志；本批语料和覆盖它的 `issue-features.json`（`index` 通过）；
- 每道题：`overlap-ledger.json`，`check` 通过（Issue、PR、同项目所有其他题都已分类，无重合）；
- `check_prompt_independence.py`、`check_prompt_style.py`、`check_prompt_rubric_scope.py --source` 全部零退出；
- 项目级 `probe.json` 记录的探针题 TraeCN 真实轮数落在 110-200；每道题在 `trials/<编号>-<项目>/` 下有绑定当前题面的 `trial-run.json`（没被熔断）和 `trial-confirm.json`（无 errors），`precheck_gate.py` 五道门禁全过。

平台预检通过后才算第一步完成，之后才进入正式执行。

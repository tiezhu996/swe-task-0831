# SWE 预检 Go 客户端

## 本地 Web 工作台

在本仓库的 `precheck-client/` 目录启动：

```sh
go run ./cmd/swe-flow serve -root ~/repositories/标注任务/swe -port 8765
```

安装 runtime 后也可用 `~/.codex/skills/swe-task/runtime/bin/swe-flow serve -root <任务根目录> -port 8765`。服务只监听 `127.0.0.1`，启动时输出实际地址；端口被占用时可传 `-port 0`。预检列表通过当前私有 SWE 凭据按需同步，页面不会读取或显示 Cookie。已有 `tools/task_dashboard.py` 继续作为只读旧看板。

“个人预检”在服务启动时预热，每 5 分钟后台同步一次，列表和预检原文匹配哈希保存在工作台私有目录的 `prechecks-cache.json`（0600）。进入页面读取缓存不会登录平台或逐题查询详情，服务重启后继续复用缓存。后台更新与缓存读取互不阻塞；失败保留旧缓存并提示，至少 1 分钟后才允许再次尝试。顶部显示缓存时间、更新中或失败状态，手动刷新可立即同步。缓存只用于列表展示及本地匹配，准备源码、复制冻结题面和正式提交仍读取真实平台记录，不以缓存授权执行。

首次使用时在页面点击“全局工作目录”，填一个评审仓库之外的绝对目录。设置保存在 `~/.local/state/swe-task/workbench/<工作台标识>/settings.json`。每条预检在全局根下建立两个平级目录：`sources/<预检编号>-<项目名>/` 是模型源码树，`workflow/<预检编号>-<项目名>/` 保存任务状态、冻结题面、提交计划及验收材料。Trae 只打开具体的 `sources/` 子目录。目录分离并非操作系统沙箱；执行后仍需审计模型实际读取的文件路径。

处理尚未执行的个人预检时，打开“个人预检”，选择“检测通过”且没有历史任务线索的条目，点击“准备独立工作区”。工作台读取完整题面与仓库语言，从原始 GitHub 仓库克隆新源码树，固定当时默认分支的完整 `HEAD`，记录完整文件清单和干净状态，并绑定预检。能读取当前平台 schema 时也自动准备本地提交计划。基线是本次准备时的实际 commit，并不假定等于预检出题时的版本；送入 Trae 前应由 Codex 核对题目仍适用于该基线。若目录或绑定中途失败，现场会保留以便诊断，不会覆盖或盲目重试。

个人预检详情提供冻结题面，供用户复制到 Trae。准备或接入源码时立即在任务状态中记录 `execution_mode=manual`，此时不声明已经执行，不填写执行开始时间或推测模型。用户在 Trae 完成题面后，打开对应本地任务，直接补充完整 Session 原串和原生 Markdown，页面不再要求点击“开始”。Session 入口会核对原始题面、准备回执及固定基线仍属于当前源码历史；执行产物可以包含源码改动或新提交，不再要求恢复为干净源码。执行前的干净检查仍在创建源码时完成，执行后的完整审计不会因此被跳过。旧版任务也可直接接收执行材料，或用 `execution-mode -state <状态文件> -mode manual` 仅登记模式，不伪造运行。

用户只需补充 Session 与轨迹。页面“复制验收提交提示词”从具体本地任务生成，携带当前状态、源码目录、固定基线、题面路径与哈希，要求 Codex 沿用现有执行做日志复核、逐项验收、终端取证、字段补齐和 dry-run，写入审核记录后停止（正式提交由用户在工作台点击），不能重跑 Trae 或启动下一题。已登记的材料不再要求重复提供。原串经标准输入送入日志核验脚本，不出现在命令参数、查询响应或交接文本；原生导出的首条 User 消息必须与冻结题面一致，且只有一条人工题面。

界面是一条统一流水线：左侧是合并后的列表（本地任务与尚未建任务的个人预检同列），每行显示仓库、难度和有效轮数；任务已有提交计划时再补两个标签：样类型（提交字段 `question_type`，如“有效轮数 < 100 且 效果好”）和时长（提交字段 `x_is_more_than_two_hours`，显示“超 2h”或“未超 2h”）。两个值只按计划里记录的原值展示，缺失时留空，不按本地时间或轮数推算；详情“材料”页同样列出。顶部用“待处理 / 等待中 / 已结束 / 全部”切换范围，“筛选”可按阶段、仓库、难度、来源和有效轮数（>100、≤100、未记录）组合过滤，并可按阶段、预检编号或有效轮数排序；筛选和排序保存在本机浏览器。右侧详情顶部只放当前阶段的一个“下一步”操作区，其余材料、题面与源码、提交字段、设计、Codex 交接放在标签页。工作目录、接入已有源码和主题切换在右上角“⋯”菜单。阶段由服务端 `stage` 字段给出。键盘：`/` 搜索，`J`/`K` 上下切换，`Esc` 返回。窄屏下列表与详情分屏切换。

路由使用 `#/tasks/<任务key>` 与 `#/prechecks/<编号>`；预检建任务后旧预检链接自动跳到对应任务。后台同步不重建正在填写的详情；状态变化以更新提示呈现。

### 提交方式与本地已完成

复制验收提示词时用“审核通过后直接提交到 SWE 平台”开关选择（本机浏览器记住上次选择，`GET /api/tasks/<key>/handoff?submit=1` 返回直接提交版提示词）：

- 关闭（默认）：Codex 完成 artifact-check、dry-run 和实据复核，写入 `workbench-review.json` 后停止。
- 开启：Codex 写入审核记录后执行 `submit -execute` 并查询回执；平台额度已满（`submission_limited`）或冻结时不重试，留在本地。

1. 审核记录与当前计划、题面、轨迹、截图和 dry-run 报告一致时，任务显示为“本地已完成”（阶段 `ready`）：列表行橙色高亮，顶部“N 项本地已完成”可一键筛出。平台状态（可提交 / 提交额度已满）显示在顶部，由 `GET /api/platform` 只读查询并缓存 3 分钟。
2. 平台开放时在详情中点击“提交到平台”并再次确认，`POST /api/tasks/<key>/submit` 实时复核平台状态、审核记录与轨迹，保存 `workbench-approval.json`，再在后台运行 `workbench-submit -execute`（最长 12 分钟）。页面轮询 `GET /api/tasks/<key>/submit` 显示进度，完成后展示平台提交编号与回执状态，或失败原因。结果同时保存在 `workbench-submit-result.json`，服务重启后仍可见；重启时仍在运行的记录显示为“结果未明”。
3. 写请求结果未明时任务进入“需核查”，只能按平台回执 reconcile，不能在页面重试。已提交任务可用 `POST /api/tasks/<key>/submission-refresh` 只读刷新回执。
历史记录（例如同编号任务状态或任务文档）会标为“历史待核查”，不能一键新跑。先复核现有执行能否交付；材料不足且允许重跑时，再由 Codex 从固定原始基线建立干净的新会话。手动“新建任务”和“使用已有干净源码”入口仍可用于已有固定基线的工作树，其源码必须干净且 `HEAD` 等于给定 commit。审计链不可读的历史任务保持只读，不能靠网页跳过校验。

网页的“提交字段”默认全部只读，可修正的字段双击（键盘 Enter）后确认解锁，保存后重新锁定。服务端解锁记录绑定任务、字段和当前 plan 哈希，15 分钟有效；跨字段、过期、重复使用或材料变化会阻断。Repo、基线、题面、Session、轮数等证据绑定项不可直接修改。Codex 完成原生终端取证、逐项 Rubric 验收和材料审核后运行 dry-run；网页授权只针对当时的 plan、题面、Session、轨迹、截图和审核哈希。任务的 `/api/tasks/<key>/handoff` 提供不含原串的交接信息。手动工作台任务按交接开关提交（见上文）。Codex 必须真实复核轨迹、目标 Session 日志、源码差异及 Rubric 证据，再在任务目录保存 `workbench-review.json`：

```json
{
  "schema_version": 1,
  "plan_sha256": "从交接信息填入",
  "prompt_sha256": "从交接信息填入",
  "trace_sha256": "从交接信息的 review_template 填入",
  "session_log_reviewed": true,
  "one_human_prompt": true,
  "source_diff_reviewed": true,
  "rubric_evidence_reviewed": true,
  "notes": "写明实际核对的输入、轨迹位置、源码差异及逐项验收证据"
}
```

只有真实核对完毕才填写这些结论。留在本地时写完即停止；用户之后在工作台点击提交，服务端等价调用 `swe-flow workbench-submit -state <task-state.json> -review <workbench-review.json> -terminal-evidence <verification-manifest.json> -trace-receipt <trae-export-sanitized.receipt.json> -tools <runtime/tools> -execute`。它先核对授权与复核记录仍对应当前材料，再由正式提交流程实时重审平台 schema、预检和 Session，创建后查询真实回执；未决写请求只能走现有 reconcile 流程。

平台 API 根路径为 `https://swe.jzxhnh.com/api/v1`。浏览器登录后使用两个 Cookie：`swe_qa_session`（HttpOnly，会话）和 `swe_qa_csrf`（非 HttpOnly，写请求要放到 `X-CSRF-Token`）。Go 程序优先调用 `Login`，让 `http.CookieJar` 自动接收 `Set-Cookie`；不要把 Cookie 写进源码、日志或提交记录。

## 预检配套设计记录与复用

出题时同步设计四项内容，使用 `DesignMaterials` 格式保存 `design-draft.json`（结构见下一节）。这是出题草稿，`origin=before_execution`、`frozen_at` 留空，原文 SHA-256 和完整固定 commit 与本题对应。运行：

```sh
swe-flow precheck-design-save -prompt PROMPT.md -plan design-draft.json -language Go
```

命令不需要已有 `task-state.json`，可传 `-schema <规则快照>`；否则只读获取当前平台规则。设计经过 Rubric、枚举、字段长度、题面/基线及研究证据校验后，保存在 `~/.local/state/swe-task/precheck-designs/records/<仓库与题面指纹>/`，研究文件按真实哈希快照保存。默认目录可用 `SWE_PRECHECK_DESIGN_DIR` 指定其他私有目录。重复保存相同设计幂等，差异设计不能覆盖；只保存本题研究摘要，单文件不超过 16 MiB，最多 32 个文件。

正常 `precheck-submit` 和 `precheck-repair` 在真正发送前自动读取任务根的 `design-draft.json` 并保存；也可复用已记录的同题面设计。缺设计时拒绝新题写入。设计仍是本地记录，平台预检请求只发送 repo_url 与 prompt。

实际通过后 `precheck-status` 自动绑定到 `approved/<编号>.json`；工作台的定时缓存同步也处理有设计记录的仓库。已有通过条目可手动运行 `swe-flow precheck-design-bind -item-id <编号>`，它实时查询平台，仓库和题面原文不一致或状态未通过即拒绝绑定。

“个人预检”列表显示“出题设计已记录”，详情可查看任务类型、真实性与难度说明、模块、Rubric、固定基线和研究依据，也支持导入 `{ "language": "Go", "materials": <DesignMaterials> }` JSON。只有尚未执行且真实预检已通过时才能登记为出题设计，已执行任务须按历史补建规则记录来源。

准备新源码时自动使用设计时的 commit，即使上游默认分支已经更新；任务计划建立后自动带入并冻结四项材料到本题 `design/`。相同原文、仓库、基线、语言及实际 schema 才能复用，不能把旧设计套到新版本。已经执行的历史任务不会被自动改写设计或源码。流程跳过重复研究，提交前的源码、日志、结果和合规审核保留。

## 设计材料与字段审核

设计预检题面时，先在任务根的 `design-draft.json` 保存四项草稿（包括 Rubric YAML 文本），不放进模型源码树。历史工作台准备计划会生成空模板，空模板不代表设计完成。题面通过预检且研究复核完成后，四项冻结设计材料统一存放在 `workflow/<任务>/design/`：`materials.json` 包含任务类型、真实性与难度说明、可能涉及模块、Verify Rubric，以及原仓库、固定基线、原文哈希、研究依据、文件证据和冻结时间；`rubric.yaml` 保存同一套验收条件。旧任务的 `rubric.txt` 或 plan 字段可以作为待核对的材料来源，不能当成新任务的冻结记录。

历史预检缺少这四项时，由 Codex 只依据冻结题面和原始基线研究补建，在评价产物前冻结，`origin` 使用 `historical_reconstruction`。不能写成执行前已设计，不能针对已观察到的产物缺陷加条款。正常设计使用 `before_execution`。设计草稿形态如下，示例只是格式，字段内容和文件哈希必须来自本题实际研究：

```json
{
  "schema_version": 1,
  "origin": "historical_reconstruction",
  "repo_url": "本题原仓库",
  "baseline_commit": "固定基线",
  "prompt_sha256": "冻结题面哈希",
  "basis": "写明原始基线研究范围、需求对应关系和验收依据",
  "source_evidence": [{"path": "基线研究文件绝对路径", "sha256": "实际文件哈希", "bytes": 123}],
  "fields": {
    "task_type": "当前平台枚举",
    "authenticity_note": "基线事实和难度依据",
    "involved_modules": "实际模块及职责",
    "verify_rubric": "rubrics YAML 原文"
  }
}
```

执行 `swe-flow design-freeze -state <任务状态> -plan <设计草稿JSON>`。工具核对基线、题面、schema、四项字段、YAML 与证据文件哈希，填入计划并记录审计链。设计已经冻结时拒绝覆盖；网页也不能再解锁这四项标准。原始设计证据应保存在模型工作区之外的私有研究快照中，不能引用会被模型继续修改的工作文件。

`swe-flow field-review -state <状态> -out <报告JSON>` 输出每个字段的约束和问题，`passed` 只表示可自动检查的规则通过。页面的“提交字段”也提供相同约束。所有 formal `artifact-check`、`dry-run`、`submit` 使用同一审核，字段缺失、错误枚举、隐私来源泄露、非法 Rubric、结果缺编号或原因、完成状态/Type/轮数冲突、错误 fork commit 链接都会阻断。正式 `SubmitWorkflow` 会再次核对设计冻结与语义证据，不能绕过网页审核直接上传。

真实性、难度、模块、实际模型、轮数、时长及实质失败还须真实语义复核。Codex 在任务根保存 `field-review-evidence.json`：`schema_version=1`、`plan_sha256` 使用 `field-review` 输出的值（不是原文件字节哈希）、`prompt_sha256` 为冻结题面哈希，`evidence` 按报告中的 `review_required` 字段逐项记录 `conclusion` 和 `sources`；每个 source 包含实际文件的绝对 `path`、`sha256`、`bytes`。记录本身不替代实际阅读和运行证据。计划或证据变化会使复核失效。

历史格式核对来自真实平台记录 #1488（QC_PASSED）及 #621、#573、#465、#447（ACCEPTED）。旧记录只帮助提炼共性，不能豁免当前 schema 和技能规则，也不复制它们的题面或结论。

## 轨迹目录与上传

每题建立 `workflow/<任务>/trajectories/`。用户可以把本题的 Trae 原生 `.md` 导出放入这个目录，网页切换“从任务目录检测”，点击“检测 Markdown 文件”并明确选择文件；也可直接“上传文件”。只列出该目录第一层普通 `.md` 文件，不读取符号链接、其他题目录或任意路径，最多 100 个，每个最大 32 MiB。检测后的文件变动会阻断并要求重新检测。

两种入口先核验 Session，再走同一轨迹原文审核。核验通过后归档到 `trajectories/archive/`，保留私有原件、脱敏 Markdown、原文与 Session 哈希回执和来源确认。只有脱敏文件进入正式附件，用户放入目录的原始文件保留。错误文件不会留下阻断重试的半成品 archive；已有有效 archive 不会被覆盖。旧任务在任务根的已绑定导出与回执继续兼容。

## 最小流程

```go
ctx := context.Background()
c, _ := precheckclient.New(precheckclient.DefaultBaseURL, nil)

// 从环境变量读取账号密码，避免硬编码。
_, err := c.Login(ctx, os.Getenv("SWE_USERNAME"), os.Getenv("SWE_PASSWORD"))
if err != nil { log.Fatal(err) }

quota, _ := c.Quota(ctx)
result, _ := c.SubmitBatch(ctx, []precheckclient.Row{{
    RepoURL: "https://github.com/owner/repo",
    Prompt:  "一段独立设计的需求题面",
}})

// 必须保存服务端返回的完整 batch_id，不要使用页面上的 8 位缩写。
progress, _ := c.BatchProgress(ctx, result.BatchID)
_ = progress
```

## 查询和返修

`ListItems(ctx, ItemQuery{Page: 1, PageSize: 20, Status: "PRECHECK_REJECTED"})` 查询列表；`ItemDetail(ctx, id)` 查询完整证据和题面。

对未通过且允许返修的记录，编辑题面后调用 `UpdateItem(ctx, id, repoURL, prompt)`，它对应页面的“保存并重新检测”，会重新走完整预检。只想使用原内容再跑一次时调用 `Rerun(ctx, id)`。两者都要先核对平台条目的仓库与绑定题面，再检查 `rerunnable`，并在之后轮询 `BatchProgress` 或查询 `ItemDetail`。

`WaitBatch(ctx, batchID, interval)` 会通过接口持续轮询到批次终态；需要在长批次中减少请求时，可使用 `WaitBatchWithOptions` 配置最大轮询间隔和 `OnProgress` 回调。`ListAllItems` 会自动遍历分页，并在服务端没有推进分页时主动报错，避免脚本卡死。客户端对 GET/HEAD/OPTIONS 的网络错误、429 和 5xx 做有限退避重试，支持标准 `Retry-After` 秒数或 HTTP 日期；提交、返修和申诉等写请求不会自动重试，避免网络超时造成重复任务。

默认每次 HTTP 尝试有 30 秒超时，整个操作仍由调用方的 `context.Context` 控制。服务端错误会返回 `*HTTPError`，可通过 `errors.As` 读取状态码和详细原因；批量脚本应据此区分登录失效、限流和服务端故障。

提交成功后可用 `NewBatchState`、`SaveBatchState` 和 `LoadBatchState` 将完整 `batch_id`、题面行指纹和创建时间原子写入权限为 `0600` 的本地 JSON。重启后先读取并核对 `RowsSHA256`，再调用 `WaitBatch`；状态文件不应上传，也不包含 Cookie、备注或日志路径。

预检编排先完成本地检查，在本次授权范围内执行写操作；`SubmitBatch` 只发送 `repo_url` 和 `prompt`，不会发送备注、轮数或本地路径字段。题面省主语只在草稿阶段使用 `NormalizeDraftPrompt`，通过预检后冻结原文，执行与提交逐字沿用。通过后需改题面时新建任务清单重新预检并执行，保留旧记录，原状态不能退回草稿覆盖。

## 轨迹上传和正式提交

`UploadAttachment(ctx, path)` 对应 `POST /uploads/attachment`，使用 multipart 字段 `file`，返回平台生成的 `name` 和 `url`。只上传脱敏后的 Trae Markdown 或其它已确认材料，不上传原始日志、截图中的本机路径或包含账号信息的文件。

`CreateSubmission(ctx, payload, patchFilePath, schemaVersion)` 对应 `POST /submissions/create`，使用 multipart 字段 `payload`、可选 `schema_version` 和 `patch_file`。方法会在发出请求前拒绝 `备注`、`remark`、`remarks`、`note`、`notes`、`patch_file` 这些内部字段；调用方必须显式构造只包含平台表单字段的 payload。上传和正式提交均是单次写请求，不自动重试。

已提供命令入口 `cmd/swe-submit`。默认本地 dry-run 不登录、不上传、不提交，按真实 schema 快照检查必填项、枚举、字段长度、附件大小与扩展名，并输出附件 SHA-256：

```sh
go run ./cmd/swe-submit -plan ../trdsql-task/submission-plan.json -schema ../trdsql-task/submission-schema.json
```

`swe-submit -execute` 已禁用，正式提交统一使用 `swe-flow submit -execute`。通过环境变量提供 `SWE_USERNAME` / `SWE_PASSWORD` 或 `SWE_SESSION` / `SWE_CSRF`；执行时重新读取平台 schema，版本变化即停止，并核对 Session、实时预检、原生终端和 Trae 导出证据。审核通过后上传附件并创建记录，失败先查询确认结果。

`ValidateSubmissionPlan` 检查必填值、占位符、类型、条件必填和附件可读性；不支持的必填 `file` 字段也会阻断。完整编排使用 `SubmitWorkflow`，它额外核对真实 Session、证据、状态链和实时预检原文。`SubmitPlan` 是旧的低层兼容接口，不能替代统一流程的证据审核与恢复保护。

`cmd/swe-flow` 的 `SubmitWorkflow` 在字段与证据审核、实时预检及附件哈希复核通过后才开始写。已有平台提交或未决操作会阻止创建。`swe-submit` 仅用于旧计划的本地字段检查，不维护完整工作流审计链。

Trae Session ID 必须在正确会话中双击**第一轮 Agent 头像区域**复制。当前 Trae CN 复制的是带多个标识、版本和时间的完整原串，不是 UUID；不得把 24 位日志 chat ID 或“未提供”当作有效字段。原串含 `no_sid` 不代表其中没有可核验的聊天会话，需要用日志定位。当前客户端只接受已实际观察到的 Trae CN 完整格式，版本改变导致格式不兼容时须重新核验并更新校验规则，不能截断原串。

复制后运行以下命令，通过真实日志、工作区和有效轮数核验；默认只读，加 `--apply` 才保存原串到本地 plan（权限 0600），并保存脱敏核验摘要：

```sh
python3 ../tools/capture_trae_session.py \
  --plan ../trdsql-task/submission-plan.json \
  --workspace ../trdsql-task/sources/task1-trdsql \
  --apply
```

`no_sid` 标志保留原样，完整原串只进入 `trae_session_id`；不写入备注、截图或上传轨迹。dry-run 和错误消息不输出完整原串。

捕获脚本默认读取剪贴板，不能通过命令参数传原串。`--apply` 保存私有 plan 和脱敏核验摘要前检查工作区、真实日志、事件数、活动会话及文件变化，权限为 `0600`。首次捕获可使用 `--update-rounds --apply --task-state <任务清单>` 从真实日志回填轮数，仅允许未提交且处于 `trae_running`/`session_captured` 的任务；之后重新 `bind-plan` 同步轮数和附件。`--task-state` 核对 plan 与 Go 状态的关联；只读复核使用 `--from-plan`，不能宣称重新从 Trae 复制。`session-verify` 和正式 `submit` 默认严格核对已有轮数，不自动更新。缺 token 只能保留本地草稿，正式提交不能填“未提供”。

已有题目必须在 plan 顶层记录 `existing_submission_id`。执行时先调用 `SubmissionDetail`，查看真实 `locked_fields`；锁定字段与本地材料不同则停止，已有记录不会再走创建接口。2026-10-05 实查 #1480 为 `PENDING_FIX`，绑定 #2409（`PRECHECK_CONSUMED`），锁定 `commit_url` 与 `trae_session_id`，平台旧值为“未提供”，因此不能通过普通返修补填。此记录有 45 个有效交互事件，平台以少于 50 轮不予收录；它仍是实际提交过的对照组，不能增加轮数或改完成状态来规避。

用户随后明确要求删除旧题并重建，已调用 `DELETE /submissions/1480`（`DeleteSubmission`，单次请求、不自动重试），历史仍保留审计。删除后 #2409 恢复 `PRECHECK_PASSED`，再次逐字核对执行提示词后通过 Go CLI 上传并创建 #1481。服务端复核确认完整 Session 原串逐字一致、两个附件齐全、无备注字段；Session ID 形态与唯一性均通过。新记录因 45 轮且完成需求仍为 `PENDING_FIX`，作为对照组保留。plan 的 `existing_submission_id` 已更新为 1481，重复执行创建会被阻止；脱敏操作回执保存在任务目录的 `replacement-receipt.json`。

若剪贴板已被其他操作更新，可用 `capture_trae_session.py --from-plan` 复核之前已捕获的原串；这个选项只做日志与工作区核验，不应写成重新从 Trae 复制。

如果认为判定有误，可调用 `Appeal(ctx, id, reason)`；平台要求申辩理由至少 10 个字符，这和“返修重跑”是两条不同流程。

`swe-flow precheck-status` 读到终态（通过、拒绝、失败）时，把打回类型、原因、`local_dup`、`issue_dup`、`difficulty_scores` 和申诉记录写进任务根 `precheck-results/<编号>-<更新时间>.json`（0600，同名不同内容拒绝覆盖，不含题面原文）。`precheck-submit` 和 `precheck-repair` 发送前要求任务根有 `scripts/precheck_gate.py` 生成的 `precheck-gate.json`：绑定这份题面、四道本地门禁全部零退出、门禁读过的文件此后没改过；`precheck-rerun` 原样重跑不需要。

## 任务状态机与 runtime

`cmd/swe-flow` 提供 `init`、`project-bind`、草稿 `prompt-normalize`、预检写入/查询/恢复、`trae-start`、`bind-plan`、Session/附件核验、dry-run、提交和状态查询。状态 JSON、审计链和附件快照通过文件锁、revision 与原子写保持关联；`advance` 不能人工声明平台通过或验证成功。现有记录通过 `import-history` 结合私有 plan 与脱敏真实回执导入，不再调用创建接口。

正式提交进入 `PENDING_FIX` 后，`submission-status` 只同步状态，不保留完整 QC 详情。表单/Rubric 返修使用技能根目录的 `scripts/repair_submission.py`：默认 dry-run，保存脱敏驳回详情和请求哈希；显式 `--execute` 才对原 submission id 做一次 PUT，随后回读 `current_version + 1`。请求结果不明时只能用 `--reconcile` 只读核对，不能重复写。

`prepare-plan` 用空模板生成新 plan，避免手工复用预检字段：

```sh
swe-flow prepare-plan -state task-state.json -plan submission-plan.template.json \
  -prompt PROMPT.md -schema submission-schema.json -out submission-plan.json
```

该命令要求 state 为 `precheck_passed`、`trae_running`、`session_captured` 或 `session_verified`，注入真实预检 ID、Repo URL、基线 commit、语言、模型和已知有效轮数；模板与 state 的非空冲突、已有输出文件都会阻断。模板附件可为空，Session 和轮数可暂缺，命令不执行完整提交审核。首次 Session 捕获先绑定空附件 plan；生成截图、Trae 导出和回执后回填附件并重新 `bind-plan`，再执行 `artifact-check`/`dry-run`/`submit`。

`schema-fetch -state task-state.json -out submission-schema.json` 通过 API 获取当前表单规则，保存私有快照并记录哈希；不会上传或建题。新建 plan 和本地审核可使用该快照，正式提交仍会重读平台规则。

所有写请求先登记 operation。超时或响应丢失后保留 `uncertain`，日志写入失败也不继续外部写。预检写前记录已有条目 ID、旧 ID/batch 和快照；恢复必须核对新 ID 或新 batch、写后时间、仓库和请求题面，同 ID 同 batch 的旧结果不能确认这次写入，缺 snapshot 的旧请求仍被阻断。提交需用 `reconcile-submission` 查询已知平台编号并匹配未决操作。

附件失败且审计明确证明 `create_request_started=false` 时，`reset-uncreated-submission` 才能回到 dry-run；平台可能留下已上传而未关联的附件。创建已开始或旧记录缺该证明时拒绝重置，只能只读查询后 reconcile，不能盲目整轮重试。平台事实不足时保留未决，等待核对。

维护入口 `tools/install_swe_runtime.sh` 将客户端、CLI、工具、计数器、困难题指南和截图 helper 打包到技能 `runtime/`；`--check` 只构建临时 staging 并运行测试，去掉该参数才发布。包不复制任务、research、plan、真实 token、Cookie 或日志。Python 3.10+、Pillow、Git/gh 和 macOS 的真实截图权限仍是环境依赖。CLI 默认定位安装 binary 旁的 `../tools`，也可用 `SWE_FLOW_TOOLS=<runtime/tools>` 显式指定，不需要回原任务工作区。

Trae 执行由用户完成，源码准备时已经登记手动模式。执行后无需调用 `trae-start`，网页可直接采集 Session 和轨迹；CLI 的 `trae-start -mode manual -model <实际模型>` 仅保留给真正需要提前记录开始时间的流程，不用于事后补造运行记录。实际模型与运行时长由日志及轨迹复核，工作台不会自动启动下一题。`collect_trae_export.py` 仅归档 UI 已导出的文件；`swe-flow next-action` 可计算下一步，`resume -until-blocked` 可连续推进本地可验证阶段，`batch-status` 可汇总多个任务；遇到桌面操作、证据缺失、未决外部写请求或正式写入时会停在闸门处，不能把 CLI 成功当作全部无人值守执行完成。

原生终端取证保存真实脚本、日志、退出码和截图的 SHA-256。验证正常结束但退出码非零可以是合法失败样本；截图完成与进程完成要分别核对，超时、截断或脚本变更不得当作完成。验证脚本的结果文件和 Rubric 总结须与日志及 exit code 的定义一致，截图内容仍需桌面复核。上传截图与 Markdown 使用脱敏材料，私有原件和完整本机路径不上传。

本轮收集优先有效轮数 `>100` 或实际持续 `>=2h` 的任务，完成和未完成都保留；不超过 100 轮样本按约 25% 收集，实现理想者保留为对照组，不计入通过样本。平台枚举、拒收和质检状态以实时读取为准，不改真实轮数或完成状态规避。

## 从已有浏览器会话接入

HttpOnly 的 `swe_qa_session` 不能从网页 `document.cookie` 读取。若必须复用浏览器会话，应在受控环境中取得两个 Cookie 后通过 `SetSessionCookies` 注入内存，且不要打印值。更稳妥的长期方案是让 Go 客户端用账号密码登录并使用自己的 CookieJar。

项目命名固定为 `<预检编号>-<GitHub仓库名>`，例如 `1528-ezbookkeeping`。新建源码目录、workflow 目录和任务标识使用同一名称，保留仓库名中的点、下划线与大小写。旧项目显示名称按同一规则生成，但既有审计身份和日志绑定路径不迁移。

平台的额度与冻结开关（`submission_limited`、`submit_frozen`）不属于表单规则：schema 绑定比较使用 `FormSchemaMatches`，额度恢复后无需重新 schema-fetch 或重做验收。`seed_model` 无特别要求时填 `Seed-Evolving`；已有未提交任务可用 `swe-flow model-set -state <state> -model Seed-Evolving` 同步 state 与 plan（会退回 Session 已核验，需重新 field-review、artifact-check 和 dry-run）。

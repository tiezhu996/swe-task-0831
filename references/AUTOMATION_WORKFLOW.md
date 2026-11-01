# SWE 自动化工作流

本指南用于 `swe-task` 的选题、预检、Trae 执行取证和正式提交。工具包放在技能自身的 `runtime/`，任务数据保留在用户指定输出目录。Go API、状态机、必填字段和附件检查已有命令实现；`swe-flow next-action` 提供可解释的下一步计算，`swe-flow resume -until-blocked` 可连续执行本地确定性阶段，`swe-flow batch-status` 可汇总任务状态。Trae 选模型、首轮头像复制 Session 和原生应用 Markdown 导出由真实桌面工具完成；正式平台写入由显式 `-execute` 控制。每一步都以实际 API、桌面状态或本地回执确认，不能把这套编排描述成完全无人值守。

## 工具与依赖

`runtime/bin/swe-flow` 为已构建的 Go CLI；`runtime/precheck-client/` 保留源码和测试；`runtime/tools/` 包含项目检索、困难题校验、Session 捕获与轮数计数、Trae 导出归档、终端取证和测试。`runtime/docs/HARD_TASK_GATE.md` 说明困难题门禁。包内包含轮数计数器及原生截图 helper 的 Swift/SH 文件，不依赖原任务工作区。

运行环境仍需 Python 3.10 以上、截图用 Pillow、Git；项目检索需要已登录的 `gh`。原生终端截图需要 macOS、Terminal、Swift 命令行工具，以及真实的屏幕录制和应用自动化权限。安装脚本只检查 Python/Pillow 与 Go，不替用户登录、开权限或安装系统软件。

以下命令假设技能根目录为 `skill_root`，任务根目录为 `task_root`，源码工作树为 `source_root`，这些变量只保存路径，不保存凭据。安装后的 CLI 默认定位 `bin/../tools`；也可显式设置工具路径：

```sh
skill_root="$HOME/.codex/skills/swe-task"
export SWE_FLOW_TOOLS="$skill_root/runtime/tools"
```

runtime 缺失时停止实际编排并报告工具未安装。修改工具后，可在维护工作区执行 `sh tools/install_swe_runtime.sh --check <技能目录>`，只构建临时 staging 并运行测试；去掉 `--check` 才会发布 runtime。安装不复制任务数据、Trae 日志、plan、Cookie 或凭据。

## 自动推进

先用 `next-action` 查看状态机给出的动作和阻断原因，再用有界的 `resume -until-blocked` 推进。`resume` 只会执行本地预检、计划生成与绑定、Session/材料核验、dry-run 和只读状态查询；命令遇到真实 Trae UI、缺失证据、未决外部写请求或正式写入时立即返回结构化原因，不会猜测、重试或绕过闸门。平台预检写入和正式提交必须分别显式传入 `-execute`，并在写请求结果不确定时先执行对应的 reconcile 命令。批量场景用 `batch-status` 汇总每个 `task-state.json` 的状态和下一步，不共享题面、plan 或证据。

## 本机私有认证

统一配置 `~/.config/swe-task/config.json`（目录0700、文件0600）存放 `schema_version=1`、`swe.username/password` 或受控 `session/csrf`、`github.login/token/transport/ssh_host/ssh_command`。它位于任务及模型工作区之外，禁止复制、cat、展示、打包或写入 Git。已授权保存凭据时，使用 `swe_credentials.py import-local --github-login <已知账号> --ssh-host <本机对应别名>` 从同目录旧 auth.json 和 gh 本地凭据安全导入；不打印值，不覆盖现有配置。后续不重复初始化。

`swe-flow` 无完整环境凭据时自动读取该配置，`SWE_TASK_CONFIG` 可指定其他私有文件；`swe_credentials.py check` 实际核验 SWE 登录、GitHub API 与 SSH 身份，只输出身份及状态。GitHub 查询可用 `swe_credentials.py run --service github -- gh api user --jq .login`；包装器仅接受 GitHub 相对端点的 GET 查询及展示/分页参数，拒绝 shell、Token 导出、其他主机和自定义请求头。平台 CLI 直接读取私有配置。

发布优先调用 `scripts/publish_task_commits.py`（runtime/tools 同版本）。工具核对 Token 身份、SSH欢迎账号、fork owner/parent，使用配置的 SSH 通道推送固定基线中的 workflow；不要只因 OAuth 无 workflow scope 就要求用户扩权。不能用错误账号、删 workflow 或改全局认证规避。准备好的 checkout 与逐分支回执保留在 `~/.local/state/swe-task/publications/`，推送使用空引用 lease，远端分支存在则拒绝覆盖；未知结果只读核对 SHA 后恢复，禁止再创建。认证过期、必要权限确实不足且无合法替代时，才简明呼叫用户；不承诺永不中断。

## 单次监控与完成后接续

首次从当前 workspaceStorage 和会话日志核定目标短 chat ID、renderer.log；后续固定绑定，不扫全部轨迹。每15分钟只运行一次：

```sh
python3 "$skill_root/runtime/tools/monitor_trae_task.py" \
  --state "$task_root/task-state.json" --checkpoint "$task_root/monitor-checkpoint.json" \
  --session-id <目标短日志ID> --log <目标renderer.log>
```

checkpoint 只存偏移、日志身份、紧凑 Git 签名和待办，不能作为提交证明，不替代 state。每次最多读取每份日志4MiB，只消耗完整行；有 backlog 下次定时继续，不同次密集循环。只按精确 session 字段识别 DoneHandler，不把内容提到另一会话 ID 当绑定。日志轮转、冲突终态或异常停止都需核查，不能自动重跑/继续。DoneHandler 是流完成候选，必须用已有 Trae 窗口实际确认首轮结束；不新开任务/窗口，不向模型输入文本。

完成后同一次运行继续：保留所有模型改动 → 冻结源码快照 → 采集原生 Markdown/完整 Session/全日志 → 核对题面、基线和已有执行记录 → 在模型目录外验收冻结 Rubric → 核对结果与附件 → artifact-check/dry-run → 写入 `workbench-review.json`，再按提示词中的提交开关留在本地或直接提交（见 SKILL.md“提交方式由交接开关决定”）。自动执行的过程证据包括唯一人工输入、完整工具读取路径及无旧答案/旧轨迹读取；用户手动执行任务遵循 [SKILL.md 的过程证据偏好](../SKILL.md)，不再将完整工具参数、计划审批原记录或执行前干净状态证据作为额外闸门。做完一个阶段后重读 state，沿用已有材料，不等下个心跳、不重复执行。`confirm_completion_then_collect_verify_submit` 的待办不会被“无变化”吞掉；已 session_verified 等阶段直接返回后续工作。豁免记录写 `waived_by_user` 并保留缺口，不冒充核验通过；其他实际阻断才需呼叫用户。不能事后伪造执行前干净证据。

监控返回 `already_submitted_no_create` 时仅核对已有回执，停用当前监控；未决写返回 `reconcile_pending_write_only` 时只读恢复，不上传/创建。通知仅限实质状态变化、完成、失败、需用户操作或作弊风险；无变化安静结束。

## 选题与草稿

用 `runtime/tools/find_swe_projects.py --language Go --limit 10 --out <新候选JSON>` 执行 GitHub 只读查询。排除表 `data/used-projects.json` 里和 workflow 里已有任务的项目会被自动剔除，列在 `excluded_used`，所以候选可能少于 `--limit`。结果仅在本次有限候选集内按公开 Issue 总量排序，包含固定基线、许可证、stars 和更新信息；不是全 GitHub 最少 Issue 排名。拉取并检出基线后运行 `scripts/screen_project.py --run-container`，`verdict=PASS` 才继续；再用 `scripts/check_issue_feature_overlap.py` 抓语料、写功能重合台账，最后读源码形成独立题面。流程和阈值见 [PROJECT_SELECTION.md](PROJECT_SELECTION.md)。公开 Issue 原文只留研究材料，不进入题面或平台字段。

困难题可参考 `runtime/docs/HARD_TASK_GATE.md` 的结构信号、题面回指、历史语料和复杂度证据。当前 `runtime/tools/validate_hard_task.py` 同时要求 A/B 两份改动及远端 commit，适用于已有 A/B 产物的辅助审阅，尚未适配全部单模型首轮出题。首轮新题不能被要求预先提供不存在的 A/B 证据，也不能虚构材料让脚本通过。已有 A/B 时可传 Git 范围复算改动；未传范围只检查声明，不证明历史或远端证据真实。该辅助检查未集成进 `swe-flow`。

题面省略主语和请求套语，只在草稿阶段归一化：

```sh
"$skill_root/runtime/bin/swe-flow" prompt-normalize \
  -prompt "$task_root/prompt-draft.txt" -out "$task_root/PROMPT.md"
```

新输出不得覆盖已固定题面。继续运行技能中的 Issue 独立性检查；归一化不证明语义、难度或独立性。预检通过后冻结原文与 SHA-256，Trae 执行、导出和提交沿用同一原文。已通过后需改题面时，新建任务清单重新预检并执行，保留旧记录；不能在原状态退回草稿、再次省主语或覆盖冻结原文。

## API 与任务状态

先 `init`，再 `project-bind`，一题一个 `task-state.json`：

```sh
"$skill_root/runtime/bin/swe-flow" init \
  -state "$task_root/task-state.json" -task task1 -workspace "$source_root"
"$skill_root/runtime/bin/swe-flow" project-bind \
  -state "$task_root/task-state.json" -repo <原始GitHubURL> -baseline <固定commit>
```

主要流转为 `initialized → project_selected → precheck_draft → precheck_submitted → precheck_passed → trae_running → session_captured → session_verified → artifacts_verified → dry_run_passed → submitting → submitted → qc_pending/qc_rejected/qc_accepted`。预检拒绝可返回草稿返修，`status` 查看当前真实状态。状态和审计使用权限 `0600` 文件、文件锁、revision、SHA-256 链及原子快照；不手工编辑 JSON 或用 `advance` 声明预检、材料审核或提交成功。

`prepare-plan` 从空模板生成新的本地提交计划，避免手工复用预检字段。先用 `schema-fetch -state <任务清单> -out <新schema文件>` 通过 API 保存当前规则；随后运行：

```sh
"$skill_root/runtime/bin/swe-flow" prepare-plan \
  -state "$task_root/task-state.json" \
  -plan "$task_root/submission-plan.template.json" \
  -prompt "$task_root/PROMPT.md" \
  -schema "$task_root/submission-schema.json" \
  -out "$task_root/submission-plan.json"
```

它只接受新输出路径，要求 state 处于 `precheck_passed`、`trae_running`、`session_captured` 或 `session_verified`，并将真实预检 ID、原 Repo URL、基线 commit、语言、已记录模型和已知有效轮数注入新 plan。模板中已有非空值与真实 state 冲突时拒绝；输出已存在也拒绝覆盖。附件尚未生成时模板的 `attachments` 可为空对象或省略，不填写不存在的路径；Session 原串和有效轮数可以暂时留空，不能伪造占位值。该命令不执行完整提交审核。

首次捕获前使用空附件 plan 绑定 state。生成 Trae 导出、终端截图和回执后，回填 plan 附件路径，再运行 `bind-plan` 计算材料哈希并同步状态，随后才执行 `artifact-check`、`dry-run` 和正式 `submit`。这样不会因为尚不存在的截图或导出文件阻断初次 Session 捕获。

`precheck-submit` 和 `precheck-repair` 发送前要求任务根有 `scripts/precheck_gate.py` 生成的、对应当前题面的 `precheck-gate.json`；`precheck-status` 读到终态时把平台判定详情存进 `precheck-results/`，打回处理见 [PRECHECK_REJECTION.md](PRECHECK_REJECTION.md)。`precheck-submit`、`precheck-repair`、`precheck-rerun` 默认 dry-run；只有授权范围内显式加 `-execute` 才发写请求。登录由 `SWE_USERNAME`/`SWE_PASSWORD` 或受控环境中的 `SWE_SESSION`/`SWE_CSRF` 提供，禁止在命令行、代码或输出中打印值。预检请求只发送原仓库和题面；正式提交只发送当前 schema 允许的表单字段。任何阶段的 payload 都不含备注、日志路径或内部诊断。

写前核对状态、仓库、冻结题面和真实平台条目；读取 `rerunnable` 不能替代身份核对。GET 查询允许有限退避，上传、创建、返修、重跑、删除、申诉不自动重试。请求超时、进程中断或平台成功后本地记录失败，都保留未决操作并阻断下一次写。

预检写前保存旧条目 ID、batch 和现有条目快照。超时后通过 `reconcile-precheck` 只读恢复：返回同题面、同 ID、同 batch 不足以证明刚才写入成功；需对应写后时间的新条目 ID 或新 batch，并核对仓库和请求题面。缺写前 snapshot 的旧未决请求继续阻断。提交通过 `reconcile-submission` 查询已知服务端编号并核对绑定字段，禁止清除 pending 后重新创建。

附件失败后，只有本地审计明确证明 `create_request_started=false` 时才能使用 `reset-uncreated-submission` 回到 dry-run；已上传未关联附件可能留在平台。创建请求已开始或旧记录缺该字段时拒绝重置，只能查询平台并 reconcile。恢复所需事实不足就保留未决，不能声称所有超时都可自动恢复。

已有平台记录使用 `import-history` 结合私有 plan 和脱敏服务端回执导入；之后只读查询或走平台允许的返修。`existing_submission_id` 或状态中的 submission ID 非零时创建被阻止。锁定字段不一致即停止；删除重建需在用户明确授权范围内执行，不由技能自动决定。

## 批量执行池

一批题全部预检通过后，由一个 Codex 会话统管执行，同时跑 2-3 个 Trae 任务。前提是每个任务各有一份源码目录（`sources/<编号>-<项目>/`），同一项目的多道题也分开克隆，都检出到各自记录的基线且 Git 干净。

```sh
python3 "$skill_root/scripts/batch_pool.py" init --root "$workflow_root" --max-parallel 3
# 每次心跳、每完成一个桌面动作后都跑一次
python3 "$skill_root/scripts/batch_pool.py" tick --pool "$workflow_root/.batches/pool-<时间>.json"
```

`init` 不传 `--task` 时按目录名取全部 `precheck_passed` 任务；传 `--task` 时按给出的顺序排队。池文件只记任务顺序和并发上限，任务状态仍以各自的 `task-state.json` 和 `monitor-checkpoint.json` 为准。

`tick` 每次按这个顺序做事：

1. 日志显示已完成或异常停止的任务，先把源码相对基线的全部改动存到该任务的 `verification/`：`frozen-source.patch`（含新增文件和二进制，不含 `.gitignore` 忽略的文件）、`frozen-source-files.sha256`、`frozen-source-status.txt` 和 `frozen-source.json`。用临时索引生成，不改模型工作区的文件、索引和分支。已有快照内容相同就跳过；不同就报 `fix_freeze`，不覆盖，先查快照之后源码是不是被人动过。
2. 检查隔离：任意两个任务的源码目录相同或嵌套、源码目录落在 workflow 目录里、待开跑任务 HEAD 不在基线或目录不干净，涉及的任务全部不得开跑，原因列在 `isolation_problems`。
3. 输出本轮动作：`fix_freeze`；`confirm_completion_then_collect_verify_submit`（桌面上确认首轮结束后，走“单次监控与完成后接续”的后续步骤）；`report_abnormal_stop`（保留现场，不续聊、不重跑，名额照样释放）；`bind_monitor`/`run_monitor`（每个运行中任务各用自己的 checkpoint 跑 `monitor_trae_task.py`，到 15 分钟才跑）；最后用空出的名额按顺序给出 `start_trae`。

标了 `desktop` 的动作要操作真实桌面，一次只做一个：给某个任务开 Trae 时，只在该任务的源码目录打开新窗口、选模型、输入冻结题面，做完立刻再 `tick`。各任务的 Session 复制、原生导出和终端截图也按队列逐个做，终端截图有全局锁排队。`pool_finished=true` 表示池里所有任务都已离开执行阶段，剩下的验收和提交照常逐题推进。

`tick --no-freeze` 只看状态不写快照。脚本不启动 Trae、不向模型输入、不写平台。

## Trae 真实桌面步骤

1. 用桌面工具打开指定源码工作区，读取当前 UI，选择用户指定模型并核对显示名称与版本；只输入通过预检的冻结题面。CLI 的 `trae-start -model` 只是记录已完成的桌面操作，不会替 Trae 切换模型。记录真实开始与完成/停止时间，不把空等待凑成执行时长。
2. 在正确会话中双击第一轮 Agent 头像区域，复制完整原串。随即运行下面的捕获命令，让脚本直接读剪贴板；原串不进入参数、shell 历史、聊天回复或终端输出。`no_sid` 保留原样，短日志 chat ID 不能替代原串。
3. 使用 Trae 原生应用的 Markdown 导出操作保存当前会话。脚本只能归档已经导出的文件，不能证明文件确由 UI 导出；保留真实桌面操作证据，不从日志或摘要拼接一个“应用导出”。

```sh
python3 "$skill_root/runtime/tools/capture_trae_session.py" \
  --plan "$task_root/submission-plan.json" --workspace "$source_root" \
  --task-state "$task_root/task-state.json" --update-rounds --apply
"$skill_root/runtime/bin/swe-flow" bind-plan \
  -state "$task_root/task-state.json" -plan "$task_root/submission-plan.json"
"$skill_root/runtime/bin/swe-flow" session-verify -state "$task_root/task-state.json"
python3 "$skill_root/runtime/tools/collect_trae_export.py" \
  --input "$task_root/trae-app-export.md" --plan "$task_root/submission-plan.json" \
  --out "$task_root/trae-export-sanitized.md"
```

捕获后立刻归档原始日志，Trae 会轮转 renderer.log：

```sh
python3 "$skill_root/scripts/trajectory_metrics.py" --workspace "$source_root" \
  --archive-dir "$task_root/trajectories/archive"
```

它只写 0600 的私有日志和度量，不改 plan 和 state，已存在且内容不同时拒绝覆盖；结果只作诊断和出题校准，不替代捕获脚本核验的有效轮数。

先 `bind-plan` 将私有草稿 plan 关联到当前状态。首次捕获使用 `--update-rounds --apply`，从正确工作区的真实日志计算并回填正整数轮数，不需先手工计数；此模式仅允许 `trae_running`/`session_captured` 阶段且不存在已有提交。回填后重新 `bind-plan` 同步轮数及当前附件哈希，再执行 `session-verify`。捕获脚本核对完整格式、工作区 `state.vscdb`、真实日志和唯一 `PlanItemHandler` 事件；日志集合、内容或活动会话在核验期间变化即阻断，不能猜整数让校验过关。

`--from-plan` 复核已捕获原串，不读剪贴板；`session-verify` 和 `submit` 会用它重新核验并以 `--apply` 写私有 plan/脱敏核验摘要。这是本地写入，不是重新复制 Session；默认严格比较已记录轮数，正式提交不加 `--update-rounds`，轮数不一致即阻断。完整 token 仅进入私有 plan 和专用 Session 字段；本地备注只记录 SHA-256、事件口径和脱敏回执。原始导出私有归档不上传；正式附件使用脱敏 Markdown，并人工复核无 token、Cookie、邮箱或本机路径且题面仍逐字一致。

## 验证与取证

完成或未完成的产物都实际运行已冻结的 Verify Rubric，不等待所有 Rubric 通过才取证。`native_terminal_evidence.py` 执行脚本，保存真实输出与退出码，脱敏后在原生 Terminal 显示真实输出并截图；它不是证明验证进程在 Terminal 里执行的 runner。可上传截图默认仅显示完整 Rubric result、简短原因、通过数、exit code 和完成状态，命令完整内容、工作区关联及 SHA-256 保留在私有 manifest 中，不把本机绝对路径暴露到上传截图。

```sh
python3 "$skill_root/runtime/tools/native_terminal_evidence.py" \
  --command-script "$task_root/verify-rubrics.sh" --workspace "$source_root" \
  --out-dir "$task_root/native-evidence" --rubric-ids 1,2,3,4,5,6,7,8,9
```

验收脚本结束时打印真实结果区：`SWE_RUBRIC_RESULTS_BEGIN`，逐行 `1 PASS/FAIL 简短原因`，最后 `SWE_RUBRIC_RESULTS_END`；兼容末尾 `Frozen rubric results:` 区。`--rubric-ids` 必须按冻结 Rubric 完整顺序指定，缺项/重复/杂乱行即拒绝精简截图；不从 exit code 推断条目通过，不隐藏失败。超过单屏的结果拒绝截尾，需缩短展示原因，详细输入、原始输出和证据仍完整归档。没有结果区的旧脚本仅能生成标明截尾的诊断图，不能当作规范 Rubric result；结果报告必须与冻结题面、Rubric、基线及源码快照逐项绑定审核，截图工具本身不证明测试语义。

后台仅运行一个验收进程，不每条 Rubric 开原生窗口。取证在私有 `terminal-window.json` 登记真实 window ID 与 `SWE Rubric Evidence` 标题，只复用这一个专属 Terminal，不使用用户其他窗口；全局文件锁保证同一时刻只有一个任务在截图，其他任务排队等锁（`--lock-wait`，默认 900 秒，超时报 `dedicated_terminal_capture_busy`）；标题变更或新增标签时停止。已登记窗口被关闭才允许重新创建。专属 shell 停用自动改标题的钩子，避免标题被覆盖导致误开窗口。显示结束立即退出，不保留300秒sleep占用；完整原始/脱敏日志继续归档，manifest v2绑定显示日志哈希，API审计核验此绑定并兼容历史v1。截图是实际原生显示，不生成仿终端图片。

已执行完成但截图缺失时，核对旧 manifest、raw/log/command 哈希，使用 `native_terminal_evidence.py --from-evidence <旧证据目录> --out-dir <全新目录> --rubric-ids <完整ID序列>` 只补原生展示与截图，不执行验证命令，不覆盖旧材料；有截断、超时、命令变更或哈希不符则拒绝。`--run-only` 可生成展示预览但不算原生截图。不得为美化截图再次跑模型、修产物或调整 Rubric。

`completed` 表示截图取证完成，`execution_completed` 表示验证进程没有超时、输出截断或执行期间脚本变更。真实验证以非零 exit code 正常结束可以收集为失败产物；它不等于证据缺失。超时、截断、脚本变更、截图缺失或权限错误属于未完成取证，不能用一张已生成 PNG 宣称完成。

审核时核对 manifest、命令、完整脱敏日志、展示日志和截图 SHA-256；查看截图中 exit code、完成状态和失败摘要与日志一致。若验证脚本生成结果 JSON/报告，保存其路径和哈希，逐条对应 Rubric，核对报告的总结与 exit code 的定义，不能仅搜到 `PASS` 就认定通过。当前通用工具未替所有项目建立结果文件语义，截图哈希也不证明内容真实；这些事实仍需真实运行和桌面复核。`--run-only` 只取日志，不能代替原生截图。

## 提交闸门

更新私有 plan 的附件路径并重新 `bind-plan` 后，按序执行 `artifact-check`、`dry-run` 和 `submit`；三者都需 `-terminal-evidence <manifest>` 和 `-trace-receipt <receipt>`。本地审核需 `-schema <快照>`，加 `-online` 可读取当前平台 schema 和预检。提交命令显式 `-execute` 才上传/创建，并再次读平台、核验 Session 和冻结附件。

闸门检查真实 schema 的必填文本、占位符、枚举、长度、条件必填、附件非空/格式/大小、Session 原串与轮数、冻结题面与实时预检、仓库与状态、材料 SHA-256、已有提交和未决操作。未支持的必填 `file` 类型也阻断，不能绕过。审计未通过时零上传零创建；低层 `CreateSubmission` 不替代这些工作流检查。

默认字段 `execution_prompt` 和内部自动字段只用于核对，不直接写 payload。`remarks`、`remark`、`note`、`notes`、中文备注、路径及内部诊断都留本地。最后查询真实提交编号和 QC 状态并保存脱敏回执；尚无服务端回执时不得报“提交成功”。

手动工作台任务按交接开关提交：关闭时 Codex 只推进到 dry-run 并写入绑定当前计划、题面和轨迹哈希的 `workbench-review.json` 后停止；开启时继续 `submit -execute`，平台额度已满或冻结则同样停在本地。留在本地的任务由工作台标为“本地已完成”；用户点击后，服务端复核审核记录与轨迹、保存 `workbench-approval.json`，在后台运行 `workbench-submit -execute`，结果写入任务目录的 `workbench-submit-result.json` 并在页面显示。`POST /api/tasks/<key>/submission-refresh` 只读刷新回执。

## 初检退回处理

`PENDING_FIX` 先读取 `GET /api/v1/submissions/<id>`，保存 `submission.reject_reason`、`qc.note` 和 checks 的脱敏摘要，再按 [Prompt 与 Rubric 加固](PROMPT_RUBRIC_HARDENING.md) 分流。只允许直接返修表单映射错误；产品缺陷要改源码并重新验收，固定基线已有等价能力的题目终止，`<100` 完成等硬性收录拒收只留本地。返修必须同步 Rubric id 与产物结果编号，写请求结果不明只读核对，不自动重试。

`swe-flow submission-status` 可能只返回状态。需要完整 QC 详情、版本和锁定字段时，使用 `scripts/repair_submission.py` 的 dry-run；它同时完成字段过滤、范围校验和脱敏归档。确认后加 `--execute`，成功必须回读原 submission id、`current_version + 1` 和 `PENDING_QC/QC_RUNNING`。若请求结果不明，只运行同一脚本的 `--reconcile`，不要再次 PUT。

`llm_rubric_scope` 点名某项 Rubric 超出原始 Prompt 时，按表单映射错误处理。只删除被点名的子句并保留同一 Rubric 的原始主行为和编号；只有整条都没有 Prompt 来源时才删除整条并连续重编号。同步重写逐项产物结果，并在原题面上用 `check_prompt_rubric_scope.py` 复核；不得改写已执行 Prompt、源码、Session 或轨迹来迁就标准。原 `design/materials.json`、`rubric.yaml` 和冻结哈希保留为历史诊断，在 `qc-repair/` 另存拒绝详情、v2 Rubric、修复说明、脱敏载荷及哈希、服务端回执，形成“原标准 → 打回原因 → 新标准 → 重提结果”的链。

返修重提应更新原 submission id，不能创建副本。平台现有接口是 `PUT /api/v1/submissions/<id>` 的 multipart 请求：`payload` 携带当前表单的非自动、非返修锁定字段，并保留现有附件 URL；同时带 `schema_version`、修复说明和 CSRF 头。先只读确认 `editable=true`、当前版本、锁定字段和原值，再提交；成功后重新读取详情，确认 `current_version` 增加、Rubric/产物结果编号闭合且状态回到 `PENDING_QC/QC_RUNNING`。403、400 等确定性拒绝表示未改动记录，保存回执并修正请求后再执行；超时或结果不明只读 reconcile，不盲目重试。

平台 checks 已全部通过但 submission 仍为 `QC_RUNNING` 时，只记录“检查结果已通过、终态未落定”。不得把 `qc.note` 的通过文本自行提升为 `QC_PASSED`，也不得通过修改真实轮数、完成状态或 Type 规避硬性拒收。

## 当前收集口径

优先保留有效轮数 `>100` 或实际持续 `>=2h` 的样本。2026-10-21 实测平台拒绝“有效轮数 `<100` 且完成状态为‘完成’”的记录，且不受 Type 或长时分类影响；此类记录只留本地诊断或对照组，不提交正式平台。`<100` 且效果差需真实核心缺陷和固定 Rubric 的稳定复现证据。持续时间与轮数分别记录，不能互相推算。现有 `type` 枚举不能表达恰好 100 轮时先读取当前 schema，仍无匹配则保留待人工确认。平台拒收时保留实际回执，不增加轮数或改完成状态。

## 工作台手动执行的设计材料与轨迹目录

准备源码时记录手动模式，执行后直接补 Session 与原生轨迹，不要求恢复源码为干净基线。轨迹放在 `workflow/<任务>/trajectories/`，网页可检测第一层 `.md` 并选择导入，也可上传；经原文核验的私有原件、脱敏文件与回执统一保存在 `trajectories/archive/`，旧根目录归档保持兼容。

四项设计材料统一放在 `workflow/<任务>/design/materials.json`，Rubric 为同目录 `rubric.yaml`。历史缺失时由 Codex 基于原始固定基线和冻结题面研究补建（Rubric 按 SKILL.md“Rubric 覆盖面”自查题面诉求与各维度），标明 `historical_reconstruction`，在评价产物前使用 `design-freeze -state <状态> -plan <设计草稿JSON>` 冻结，不针对产物倒推或修改标准。字段结构与证据格式见 runtime/precheck-client/README.md。

提交前运行 `field-review`，修复逐字段结构问题，再保存与报告 `plan_sha256` 匹配的 `field-review-evidence.json`，为 `review_required` 字段提供实际源码、日志和验收文件的结论及哈希。正式审核和提交同时检查冻结设计、逐字段格式和语义证据，不能把格式检查通过写成事实已核实。

字段证据的 `sources` 只能引用在提交预检期间不会变化的文件。`submit` 会在语义复核之前重新核验 Session 并刷新 `submission-session-audit.json`；把该文件（或 `task-state.json` 等同步文件）列为 source，会让原本通过的 artifact-check 在首次真实提交时变成 `field_semantic_review: missing_or_changed_field_evidence`，并因证据链已过期而不上传、不创建。模型、时间、轮数和 Session 证据应引用独立的 `verification/execution-evidence.json`、归档私有日志、`trajectory-metrics.json`、原生导出回执和提交计划；如果必须引用会变化的材料，应先制作不可变快照，不能靠提交前反复重算哈希绕过。2026-10-27 的 5123-ggshield 首次提交已实际复现，改用不可变执行快照后第二次提交创建成功。

工作台项目命名固定为 `<预检编号>-<GitHub仓库名>`，例如 `1528-ezbookkeeping`。新建的任务标识、sources 子目录与 workflow 子目录同名；历史已绑定任务只统一展示名称，保持实际日志与审计路径。

## 出题配套设计记录

新题定稿时同步生成四项 DesignMaterials，运行 `precheck-design-save -prompt <题面> -plan <design-draft.json> -language <语言>` 存入私有设计库。新预检写入会先检查及保存设计，缺失即阻断；平台只接收原仓库和题面。预检通过后真实绑定编号，工作台后台缓存同步及 precheck-status 自动处理，已有通过编号可用 precheck-design-bind。之后准备任务会检出设计时的基线，并在任务 design 目录自动复用冻结四项材料。不要把历史任务执行后补建的材料登记为出题设计，也不要根据模型结果调整原设计。详细格式及路径见 runtime/precheck-client/README.md。

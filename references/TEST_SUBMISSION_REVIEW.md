# Test 包提交前复审

本机制适用于任何对外发送或平台提交的 `<task-id>-testzip.zip`。静态工具只判包结构与可复现的机械条件，不能替代测试语义审核；静态结果 `STATIC_PASS` 不等于提交通过。

## 复审步骤

1. 冻结本次复审对象：题号、原仓库、基线 commit、历史候选 commit、最终 ZIP、答辩文档。写明 ZIP 与答辩文档的 SHA-256。
2. 准备干净的基线仓库副本，HEAD 必须等于 `config.json.base_commit` 且工作树干净。运行 `scripts/review_test_bundle.py`，要求最终 ZIP 与待打包 `tests/` 逐文件一致、成员只在 `tests/` 下、补丁可应用、配置和脚本检查通过。保存 JSON 报告。
3. 从最终 ZIP 解压到新目录；以解压副本运行后续探针，不使用 staging 副本代替。确认解压目录与被检查的 ZIP 文件哈希一致。
4. 在声明的目标环境运行基线、完整参考实现和历史候选。保存确切命令、镜像/系统标识、原始 stdout/stderr、退出码和逐项报告。候选断言失败可以是有效结果；不能把它改写为环境失败。基线因接口缺失而编译失败时记录“用例未执行”，不能声称每条 F2P 都实际失败。
3a. 必查清单（造成缺漏最多的四项）：Rubric 逐名词核对表已建立；报告由被测命令真实生成并多次重复扫描；摘要计数与夹具真值逐项相等；P2P 已在基线上按默认顺序、单独运行、乱序三种方式确认稳定。
4a. 对照平台历史记录：历史“产物结果”中每个未通过的 Rubric，都要在历史候选的运行中找到对应的失败用例；找不到即 `REWORK`。测试判定的失败项多于历史记录时，逐条附上复现输入和原始输出。
4b. 写入隔离探针：在历史候选上追加一个 `init()`，尝试改写 `/tests` 下的 grader/config/test.sh 并在 verifier 目录伪造 reward；用未修改的入口在目标镜像中运行，必须看到写入被拒、资产摘要不变、reward 由真实结果决定。
4c. P2P 防篡改探针（2026-10-08）：确认所有 `go test -c` 都在基线 + test.patch 的干净目录中执行，评分二进制不链接任何提交代码。在目标镜像用未修改入口跑三个探针：改坏被回归覆盖的业务函数（P2P 必须下降）；再同时禁用测试辅助断言（仍须下降）；提交包加入“在 `*.test` 进程内即 panic”的 `init()`（各套件须正常完成）。三者的 F2P 应与无探针候选一致，否则探针无效。黑盒回归另需本机业务变异灵敏度证据，以及在基线、候选、参考二进制上全部通过的结果。
4d. 中断一致性（恢复/迁移/修复类）：kill 之后、重跑之前先正常启动被中断的实例，要求恰好是原实例或恰好是完整目标状态，否则拒绝启动并指出未完成的操作；参考实现上配“不处理恢复日志”“只在重跑时处理”两个变异，均须被拦截。
4e. 未关闭项清单：列出历次审核报告的全部阻断项，逐条写“已修（证据）/不修（理由）”；共享同一基线或入口的兄弟任务逐条交叉复核。机器可读协议判定器须递归检查 summary/final，并在测试内带判定器自检样例。
4f. 公平性与公开契约（2026-10-08 644 打回后补充）：逐项检查测试是否把参考实现的未公开选择写成硬契约。最低集合是三个等价实现变体——规则顺序改成首次出现顺序、JSON 枚举取值改成另一套同义值、错误文案改成另一套明确措辞——三者都必须与参考实现同分通过。排序只允许断言同一实现多次运行的顺序稳定；枚举只允许断言入口可区分且字段非空；错误只允许断言非零退出与可定位输出。证据写入 `fairness-evidence.json`，至少含 `order_variant`、`enum_variant`、`error_variant`、`p2p_import_export`，每项 `result=pass`、`reward=1` 并附真实文件。P2P 还要覆盖题面明确提到的导入/导出/摘要入口；644 这类任务必须实跑 `sync export`→目标环境 `sync import`→读回最终状态。`test.patch` 必须被 `test.sh` 通过 `git apply` 或 `patch -p1` 实际消费，评分测试二进制从 `git archive <基线> + test.patch` 的 pristine 树编译。

4g. 错误质量、JSON 字段与构建独立（2026-10-08 644 v2 打回后补充）：错误场景不能只断言非零退出和 stderr 非空。用 `error_vague` 探针验证无信息错误（例如只输出 `failed`）被拦截，用 `error_panic` 验证 panic/stack trace 被拦截；错误输出至少要含目标标识或可识别原因词，但不绑定具体句子。JSON 字段名按仓库既有公开约定做别名匹配：仓库文档已用 `hits[].count/snippets` 时，测试必须同时接受 `rules`/`hits`、`kind`/`type`、`snippets`/`matches`；`json_field_alias` 等价实现必须 reward=1。入口不得硬依赖 `/app` 的 `.git`：优先 `git archive <基线> + test.patch`，但必须提供交付包自带 `deliverytests` 的 `TEST_SOURCE=packaged` 回退；`git_fallback` 探针要求无 git 源码导出时正确候选仍 reward=1。`patch_source_sync` 检查 test.patch 注入的 Go 源与 deliverytests 是否逐字一致，漂移或死件即 HOLD。

4h. 通用断言有效性与交付卫生（2026-10-08 644 v3 打回后补充）：先建立独立输入真值，再检查断言能否被输入变化区分。每个检测/统计/报告功能至少要有零命中负例、多变体正例、多类型或多数量正例；计数与类型必须和独立路径（索引、share、导出归档、读回状态）交叉核对，不能只比较候选自己的输出。`stub_output`（固定输出）、`merged_kinds`（合并类型/计数漂移）和 `error_generic`（“file error”/“session error”）三个负向探针必须 reward=0；`zero_hit`、`multi_kind_counts`、`existing_test_regression` 三个正向探针必须 reward=1。Rubric 提到既有测试时，入口必须实际执行相关测试或给出逐项黑盒替代映射。ZIP 不得包含 `__pycache__`、`*.pyc/*.pyo`、`.DS_Store`、对象文件或构建二进制；`cache_artifact_scan` 命中即 HOLD。

4i. 进程级只读、错误双条件与多行 JSON（2026-10-08 644 v4 打回后补充）：需求写“不写任何文件”时，把 TMPDIR、HOME、cwd 全部放进测试沙箱并整树哈希；只快照 fixture 根目录会让 os.CreateTemp 漏判，`tempfile_write` 必须 reward=0。错误必须同时含失败目标和具体原因，只回显目标不算，`target_echo_only` 必须 reward=0；无目标错误至少要有明确 require/usage 语义。机器可读输出解析必须接受多行/pretty JSON 和前后诊断，`pretty_json` 等价实现必须 reward=1。公平性证据新增正向 `pretty_json`、负向 `target_echo_only` 与 `tempfile_write`。

5. 对 grader 做与实现风险相符的反向探针。至少验证通过、单项失败、失败后包级失败、构建失败、重复或乱序执行；报告类需求还要分别试非法内容和合法空/缺字段对象；回执类还要试“逐行全部通过但 harness 声明 execution_error/退出码 2”。对实现做变异探针时，同时做“合理替代实现应通过”的误杀探针（例如 Rubric 允许的安全失败），并证明每个变异确实执行到了被改的分支。保留输入、输出、退出码及 SHA-256。
6. 逐条将冻结 Rubric 映射到具体测试名、输入夹具、断言结果及运行证据。覆盖为 `full`、`partial` 或 `missing`；有任何 `partial`/`missing` 时结论只能是 `HOLD`，除非需求方书面缩小验收范围并重新冻结 Rubric。
7. 第二人对最终 ZIP、逐项映射、失败结果、答辩文档和所有引用的证据哈希进行复核。最终复审后只要改了 tests、grader、config、Dockerfile、test.sh、Rubric 或答辩结论，就重新打包并从第 2 步复审。

## 自动静态检查

为每个任务准备独立干净基线副本，然后运行：

```sh
python3 "$skill_root/scripts/review_test_bundle.py" \
  --tests-dir /绝对路径/工作区/题号/tests \
  --zip /绝对路径/交付目录/题号-testzip.zip \
  --base-repo /绝对路径/干净基线仓库 \
  --base-commit <config.json 中的 base_commit> \
  --defense /绝对路径/交付目录/题号-defense.md \
  --fairness-evidence /绝对路径/复审证据/题号-fairness-evidence.json \
  --out /绝对路径/复审证据/题号-static-review.json
```

脚本校验 ZIP 完整性与路径、ZIP/目录逐字节一致、必需文件、配置 ID 唯一与数量、run 目录报告路径、Shell/Python 语法、禁用文案、答辩题号、基线补丁适用性、`test.patch` 是否被入口消费、未公开契约字面量绑定，以及公平性/导入导出证据、字段别名、无 git 回退、错误 vague/panic/generic 负向探针、零命中/多类型/pretty JSON 正例、stub/merge/target-echo/tempfile-write 反作弊探针、TMPDIR/HOME/cwd 只读覆盖、缓存与构建产物和 test.patch/deliverytests 同步是否齐全。脚本只输出 `STATIC_PASS` 或 `HOLD`，报告固定包含 `submission_ready: false`；这是刻意设置的语义闸门，不能把机械检查伪装成完整验收。

## 结论规则

- `READY`：静态复审通过；目标环境和参考实现的真实运行证据齐全；针对性错误实现被测试拦截；每条 Rubric 均为 `full`；第二人签署且所有证据哈希匹配。
- `HOLD`：真实候选/参考实现、目标容器、失败探针、逐条映射、附件或第二人复核任一缺失；禁止以测试总分、历史任务已通过或静态 `STATIC_PASS` 替代缺失项。
- `REWORK`：发现误放行、错误统计、测试顺序依赖、包与 staging 不一致、Rubric 漏测、证据与文档不符、压缩包中混入非测试内容、历史未通过项未被复现、候选可写判分资产、评分测试二进制链接了提交代码（含仅恢复基线 `*_test.go` 的做法）、中断后启动可见半套状态、历次审核报告的阻断项未逐条关闭，或测试绑定了题面未约定的排序、枚举值、错误文案/内部符号而答辩没有公开该契约；`test.patch` 未被入口消费；公平性等价实现被误杀；题面明确提到的导入/导出入口没有 P2P 覆盖；无信息错误或 panic 被计为通过；固定输出 stub、合并类型或计数漂移被计为通过；错误只回显目标或只用泛化名词；只读检查不覆盖 TMPDIR/HOME/cwd；解析器拒绝合法多行 JSON；Rubric 声称既有测试通过但入口未执行或未给黑盒映射；交付混入缓存/构建产物；JSON 字段名硬绑定参考实现而不接受仓库既有命名；因 /app 无 .git 让正确候选 execution_error；test.patch 与 deliverytests 漂移或留下死件。修复后从头复审最终 ZIP。

只有填完 [复审记录模板](TEST_PRE_SUBMISSION_REVIEW_TEMPLATE.md) 并得到 `READY`，test 包才可交付或提交平台。工具不执行上传或平台写入。

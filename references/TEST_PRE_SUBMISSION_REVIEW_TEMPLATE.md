# Test 包提交前复审记录

## 对象

- 题号：
- 题目名称：
- 原仓库：
- 基线 commit：
- 历史候选 commit：
- 最终 testzip：
- testzip SHA-256：
- 答辩文档：
- 答辩文档 SHA-256：
- 目标执行环境/镜像 digest：
- 自动静态报告及 SHA-256：

## 运行证据

| 对象 | 命令与环境 | 结果 | 退出码 | 日志/报告路径及 SHA-256 | 复核说明 |
| --- | --- | --- | ---: | --- | --- |
| 基线 |  |  |  |  |  |
| 完整参考实现 |  |  |  |  |  |
| 历史候选 |  |  |  |  |  |
| 错误实现/报告变异 |  |  |  |  |  |
| 失败后包级失败/构建失败 |  |  |  |  |  |
| 陈旧 reward/空报告/退出码探针 |  |  |  |  |  |
| 回执矛盾（全通过但 execution_error/退出码异常） |  |  |  |  |  |
| 写入隔离（候选 init 改写 grader/伪造 reward） |  |  |  |  |  |
| 误杀探针（合理替代实现、Rubric 允许的安全失败） |  |  |  |  |  |
| P2P 探针①：改坏被回归覆盖的业务函数 |  |  |  |  |  |
| P2P 探针②：①＋禁用测试辅助断言 |  |  |  |  |  |
| P2P 探针③：提交包 init 在 *.test 进程内 panic |  |  |  |  |  |
| 黑盒回归灵敏度变异（本机）与基线/候选/参考二进制通过情况 |  |  |  |  |  |
| 中断后启动一致性（含“不处理恢复日志”“只在重跑时处理”变异） |  |  |  |  |  |
| 默认顺序/单独运行/乱序运行 |  |  |  |  |  |
| 公平性等价实现：规则顺序 |  |  |  |  |  |
| 公平性等价实现：JSON 枚举值 |  |  |  |  |  |
| 公平性等价实现：错误文案 |  |  |  |  |  |
| 导入/导出/摘要 P2P 黑盒覆盖 |  |  |  |  |  |
| test.patch 被入口消费 |  |  |  |  |  |
| JSON 字段别名等价实现 |  |  |  |  |  |
| 无信息错误负向探针 |  |  |  |  |  |
| panic/stack trace 负向探针 |  |  |  |  |  |
| /app 无 .git 时的 packaged 回退 |  |  |  |  |  |
| test.patch 与 deliverytests 同步 |  |  |  |  |  |
| 零命中负例 |  |  |  |  |  |
| 多类型/多数量正例 |  |  |  |  |  |
| 既有脱敏测试/黑盒替代映射 |  |  |  |  |  |
| 固定输出 stub 负向探针 |  |  |  |  |  |
| 合并类型/计数漂移负向探针 |  |  |  |  |  |
| 泛化错误文案负向探针 |  |  |  |  |  |
| 缓存/构建产物扫描 |  |  |  |  |  |
| TMPDIR/HOME/cwd 只读快照 |  |  |  |  |  |
| 多行/pretty JSON 正例 |  |  |  |  |  |
| 只回显目标错误负例 |  |  |  |  |  |
| TMPDIR 临时文件写入负例 |  |  |  |  |  |

## 公平性与公开契约

| 探针 | 变体改动 | 预期 | 实际结果 | 证据路径及 SHA-256 | 结论 |
| --- | --- | --- | --- | --- | --- |
| order_variant | 规则顺序改为首次出现顺序 | 与参考实现同分通过 |  |  | pass/fail |
| enum_variant | target.type/source 改成另一套同义枚举 | 与参考实现同分通过 |  |  | pass/fail |
| error_variant | 错误文案改成另一套明确措辞 | 与参考实现同分通过 |  |  | pass/fail |
| p2p_import_export | sync export→目标环境 sync import→读回最终状态 | 退出 0，记录可读回，秘密不泄漏 |  |  | pass/fail |
| json_field_alias | rules→hits、kind→type、snippets→matches 等仓库既有命名 | 与参考实现同分通过 |  |  | pass/fail |
| git_fallback | /app 为无 .git 源码导出 | 正确候选仍 reward=1 |  |  | pass/fail |
| error_vague | 错误只输出 failed，不含目标/原因 | reward=0，F2P3 fail |  |  | pass/fail |
| error_panic | 缺失输入时 panic 并打印栈 | reward=0，F2P3 fail |  |  | pass/fail |
| error_generic | 只输出 file error / session error | reward=0，F2P3 fail |  |  | pass/fail |
| stub_output | 固定 kind/count/snippet，不读输入 | 与输入不一致，reward=0 |  |  | pass/fail |
| merged_kinds | 把多个真实类型合并为一个 | 与独立真值不一致，reward=0 |  |  | pass/fail |
| zero_hit | 无秘密输入 | 0 命中且 reward=1 |  |  | pass/fail |
| multi_kind_counts | 多类型、多数量输入 | 计数/类型与独立路径一致，reward=1 |  |  | pass/fail |
| existing_test_regression | 既有测试实际执行或逐项黑盒替代 | reward=1 |  |  | pass/fail |
| pretty_json | 多行/pretty JSON + 前置诊断 | 能解析并 reward=1 |  |  | pass/fail |
| target_echo_only | 错误只回显目标，无原因 | reward=0 |  |  | pass/fail |
| tempfile_write | 在 TMPDIR 写临时文件 | 只读沙箱检出，reward=0 |  |  | pass/fail |

## 历次审核报告未关闭项

| 报告 | 阻断项 | 本版处理（已修/不修） | 证据或理由 | 兄弟任务是否同样存在并已处理 |
| --- | --- | --- | --- | --- |
|  |  |  |  |  |

## Rubric 逐名词核对

| Rubric ID | 原文名词/要求 | 断言所在测试 | 断言内容 | 无断言时的缺口说明 |
| --- | --- | --- | --- | --- |
|  |  |  |  |  |

## P2P 基线稳定性

| P2P 用例 | 基线默认顺序 | 基线单独运行 | 基线乱序 | 运行方式（同进程/单独进程）与处理 |
| --- | --- | --- | --- | --- |
|  |  |  |  |  |

## 历史结论对照

| Rubric ID | 历史产物结果 | 本包对历史候选的判定 | 对应用例 | 一致/差异说明与证据 |
| --- | --- | --- | --- | --- |
|  | 通过/未通过 | 通过/失败 |  |  |

## Rubric 映射

| Rubric ID | 测试名称 | 输入/夹具 | 预期断言 | 实际结果 | 证据路径及 SHA-256 | 覆盖结论 | 缺口/说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
|  |  |  |  |  |  | full/partial/missing |  |

## 复审结论

- 未解决问题：
- ZIP 与 staging 最终逐文件比对：通过 / 未通过
- 是否存在未执行或未验证条目：
- 复审人：
- 复审时间：
- 结论：HOLD / REWORK / READY
- READY 签署说明：

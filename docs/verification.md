# 验证记录

## 2026-10-14：基础工程与持久 core

截至本节，OpenSpec 1.1–2.5、3.1–3.4 已完成，共 13/68 项。Day1 整体仍在实施，模型循环、完整 TUI、四协议、MCP/插件和部署验收尚未完成。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 包依赖隔离 | `TestCoreDoesNotDependOnUI` | 核心依赖图不含 Bubble Tea/render |
| 协议契约 | `internal/protocol/*_test.go` | 版本、编码往返、未知/重复字段、稳定请求指纹通过 |
| 配置 | `internal/resource/config_test.go` | 层级、来源、profile、非法类型、凭据引用及输出检查通过 |
| 事务与只追加 | `TestAtomicRollbackAndHistoryGuards` | 注入失败后任务与事件回滚，历史改写/删除被拒绝 |
| 请求去重 | `TestDurableAcceptanceAndDedup`、`TestConcurrentRetryAndCursorBoundary` | 并发重试、重开数据库后身份不变 |
| 状态/事件一致性 | `TestSnapshotRacesTaskCommits`、`TestSubscriptionReconnectAtSnapshotCursor` | 快照游标一致，断线补发不重提任务，超前游标报错 |
| 独立后台进程 | `TestDetachedCoreBinary` | 真实二进制并发启动只产生一个 core；客户端退出后仍可查，重启后接受记录不重复 |
| core 崩溃后的 pipe | `TestDetachedCoreBinary` | SIGKILL core 后 runner/pipe 身份不变；新 core 查询原进程，释放测试 gate 后取回原退出结果和输出 |
| pipe 字节与身份 | `internal/process/manager_test.go` | 瞬时输出、重复 process ID、空输出、超限缺口、PID 复用识别通过 |
| 真实 PTY | `TestPTYRealTerminalInstantExitAndIdentity` | stdin/stdout/stderr 均为终端，91×31 尺寸、原始 CRLF、退出码 7 和启动去重通过 |
| PTY 采集 | `TestPTYBurstEmptyAndOutputGap`、`TestPTYMissingStoredRangeIsExplicit` | 空输出、256 KiB 突发、1 KiB 保存额度、缺口和文件损坏报错通过 |
| PTY 重连与身份 | `TestPTYReopensRunningAndCompleted`、`TestPTYServerLifetimeMismatch` | 重开 runner 接回活动/完成记录；server 生命周期不匹配返回 unknown |
| runner 崩溃后的 PTY | `TestDetachedCoreBinary` | SIGKILL 真实 runner 后 tmux/采集继续；新 runner 接回同一身份，返回完整 beforeafter 和原退出码 9 |
| tmux 依赖 | `TestPTYIsNotSilentlyPiped`、`TestPTYInvalidDependencyAndSize` | 缺失/不可执行明确报错，不启动 pipe；非法尺寸被拒绝 |
| stdin 与关闭 | `TestPipeOrderedInputCloseAndDurableDedup`、`TestPTYInputResizeTargetAndCanonicalEOF` | 原进程顺序输入、二进制字节、pipe 关闭、PTY canonical EOF、持久去重通过 |
| resize 与 raw mode | `TestPTYInputResizeTargetAndCanonicalEOF`、`TestPTYRawCloseIsExplicitlyUnsupported` | 内核尺寸到达目标，另一 PTY 保持 80×24；raw mode 不伪报关闭 |
| 取消与组终止 | `TestSignalCancelAndOwnedGroups` | SIGINT trap 执行；pipe 组和 PTY 独立 job-control 组结束，真实终态可查询 |
| 控制不确定性 | `TestControlPendingIsNotReplayedOrBypassed`、`TestPipeWriteTimeoutIsBoundedAndReportsPartial` | pending 不重放且换 ID 不绕过；部分写入有记录，超时不杀命令 |
| 控制 CLI | `TestProcessCLIControlAndBinaryInput` | 文件/stdin、重复请求、控制查询、帮助与 JSON/诊断分流通过 |
| CLI | `internal/cli/cli_test.go` | help/version、文件/stdin、长中文、stdout JSON、stderr 错误通过 |
| 限额与重试 | `TestLimitErrorPreservesExistingState`、`TestRetryBoundedAndUnknownMethod` | 达限保留已有工作，连接重试有上限 |

当前代码的全量 `go test ./...` 和 `go test -race ./...` 已通过，OpenSpec strict 校验通过。一次真实二进制构建和一次 race 构建受共享 Go 缓存文件消失影响，报编译器或依赖文件不存在；这些失败不计为通过。后续成功结果使用独立工具链/缓存。

控制测试曾用 `File.Fd()` 调小 pipe 缓冲，意外将测试管道改为 blocking，旧版 race 夹具失败。已改用 `SyscallConn.Control`，修正后的普通测试和全量 race 均通过；旧失败不计为通过。

隔离验证命令：

```sh
env GOMODCACHE=/tmp/zende-build-cMt4sx/mod \
    GOCACHE=/tmp/zende-build-cMt4sx/cache \
    GOTOOLCHAIN=go1.26.5 go test ./...
```

当前实际编译工具链为 Go 1.26.5；PTY 最低已测 tmux 为 3.3a。基础 PTY 用例连续运行五次通过，新增恢复/损坏用例及全量 race 测试通过。Go 依赖完整版本见 `go.mod` / `go.sum`。

真实二进制测试使用独立临时 XDG 目录，并停止测试创建的 core；没有启动默认 profile 的生产服务、写入模型凭据或修改参考仓库。

## 2026-10-15：输出保留与日志投影

3.5 已完成，累计 14/68 项。全量 `go test ./...` 和 `go test -race ./...` 通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 引用保护 | `TestRotationPreservesTaskAndHistoryReferences` | Task 尚未释放的输出与历史引用均保留，预算不足不删除；重开后引用和轮转结果仍在 |
| 活动进程与并发 pin | `TestActiveOutputAndConcurrentReferenceAreProtected` | 活动输出保留，引用与删除竞态中不丢失已成功 pin 的内容 |
| 中断轮转 | `TestRotationResumesOriginalPlanAfterTombstone` | 释放标记已保存但尚未删除时重开，仅续原清单，后来输出保留 |
| PTY 引用 | `TestPTYOutputReferencesAndRotation` | 真实 PTY 完成后仍受 Task 引用保护，释放后可清理，重开仍可查询原退出结果与 gap |
| 缓存 | `TestOutputCacheBoundedAndCorruptionNotHidden` | LRU 有界且无数据别名，文件截断报错，同大小/时间替换不返回旧缓存 |
| 控制字节 | `TestDisplayTextEscapesTerminalControlAndBoundsFrame` | 原始字节保留，ESC/OSC/C1/双向控制/非法 UTF-8 转义，最大读取帧未超 IPC 限额 |
| CLI | `TestProcessCLIControlAndBinaryInput` | pin/unpin、预算查询、dry-run、删除确认、持久结果查询和重复请求通过 |

## 2026-10-15：进程故障恢复

3.6 已完成，累计 15/68 项。修改后的全量普通测试和 race 均通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 真实 runner 崩溃后的 pipe | `TestDetachedCoreBinary` | SIGKILL runner 后原 pipe 仍在，重试原 ID 返回 unknown；前缀保留、stdin 拒绝、效果只发生一次；可终止原组，再次重开识别为 lost，不伪造退出码 |
| 已完成 PTY | `TestDetachedCoreBinary` | runner 再次重启后，保留原退出码 9；已有 core/PTY 崩溃续接路径仍通过 |
| 损坏隔离 | `TestRecoveryQuarantinesOnlyDamagedRecord` | 缺失/非法 JSON/未来版本/身份不符/非法 stream/非普通文件只隔离原记录，不覆写原文件，不重用 ID，不影响其他进程 |
| 派发窗口 | `TestRecoveryDispatchWindowsNeverRerun` | 确定性落盘夹具覆盖 pipe 意图、效果已发生但身份未存、PTY 意图；重开保留 unknown，原操作不重复 |
| 丢失范围 | `TestPipeRecoveryPreservesMissingRange` | pipe 恢复不缩短已知字节数，文件截断保持显式错误 |
| PTY 采集故障 | `TestPTYCaptureAndServerLoss` | 真实终止采集器/private server 后保留 unknown 和已确认输出；非法计数不替换旧元数据；原命令不重跑 |

复现命令和恢复边界见 [进程 runner](./processes.md#恢复边界)。这些测试覆盖 V07、V13 的进程部分：没有据此宣称 Agent 层待核实屏障、TUI 接管或整套 V13 已交付。

## 2026-10-15：单 Agent 模型循环

4.1 已完成，累计 16/68 项。全量 `go test ./...`、`go test -race ./...` 通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 两轮工具与错误回填 | `TestLoopTwoToolRoundsErrorFeedbackAndPersistence` | 三次模型请求、两轮工具，第二轮错误进入下一请求，显式 final 保存；重开数据库可查原上下文 |
| 单会话串行 | `TestSameAgentSerialDifferentAgentsConcurrent`、`TestClaimNextSerializesAndCommitsContext` | 同一 Agent 只领取一个任务，排队输入不提前进入上下文；不同 Agent 并行 |
| 客户端断开 | `TestClientDetachDoesNotCancelModelLoop` | 真实 Unix socket 接受任务后取消客户端上下文，后台模型循环仍完成 |
| 参数与传输边界 | `TestStreamFailuresNeverDispatchPartialCalls` | 分片未完成、重复 JSON 字段、完整事件后传输报错都不派发工具；已收到片段保留，普通错误不泄漏传输细节 |
| 公共模型事件 | `TestCollectorInterleavedCallsAndPrivateData`、`TestCollectorRejectsIncompleteAndInvalidEvents` | 交错调用、参数合并、私有数据归属、非法顺序及组装限额通过；缺失用量保持未知 |
| 显式 final | `TestFinalIsExplicitAndConflictsAreRejected` | 响应结束不推断 final；重复 final、后续正文及 final/tool 冲突被拒绝 |
| 未知与故障隔离 | `TestUnknownToolOutcomeHoldsAgentQueue`、`TestUnknownToolAndPanicIsolation` | 未知效果阻止后续队列，未注册工具作为错误回填；执行器 panic 保留 unknown，其他 Agent 仍能完成 |
| 事务与重开 | `TestResponseCommitDoesNotLeavePartialContext`、`TestInterruptedLoopIsHeldOnceWithoutReplaying` | 响应提交失败不留下半段上下文；重开将旧循环置为待核实，不重派原调用 |
| 工具快照 | `TestRegistryOwnsDescriptorsAndRejectsInvalidDefinitions` | descriptor 独立复制，调用期间不受调用方改写，重复或非法定义被拒绝 |

模型来源为 `internal/model/fixture`，工具为确定性测试执行器。真实进程接线、文本标签修复和真实 provider 尚未由本节验证；普通 CLI 的模型状态仍为 unavailable。Pi 参考的提示词已用于这些夹具请求，来源及边界见 [提示词](./prompts.md)。

## 2026-10-15：模型进程工具

4.2 已完成，累计 17/68 项。全量 `go test ./...`、`go test -race ./...` 通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 模型到独立 runner | `TestLoopExecAndContinueRealRunner` | 确定性模型经真实 Unix socket 启动 pipe/PTY、续写原进程、读回结果并完成；派发前已有持久意图，命令只执行一次 |
| 参数中途断流 | `TestIncompleteArgumentsNeverReachRealRunner` | 任务报错，runner 未创建进程，目标文件不存在 |
| 续写与去重 | `TestProcessToolsContinueOriginalPipeAndDeduplicate` | 长进程返回身份，输入和重复调用不重复副作用，Task 引用保留 |
| 结果与输入校验 | `TestProcessToolOutputCursorsAndControlEscaping`、`TestProcessToolsValidateBeforeStartingOrWriting` | 游标、退出码、缺口及控制字符投影正确；非法参数在派发前拒绝 |
| PTY 与目录 | `TestProcessToolPTYAndRelativeCwd` | 真实 TTY、88×29 尺寸、相对 cwd 和持续输入通过 |
| 不确定性 | `TestObservationFailureRetainsAcceptedProcessIdentity`、`TestRetryDoesNotDowngradeUnknownDispatchToOffline`、`TestMutationMalformedReplyRemainsUnknown` | 读回失败保留身份；已派发请求遇到离线、损坏响应仍标为未知 |
| 部分输入 | `TestRemoteInputRetainsPartialWriteAndReplayResult` | 真实 pipe 写入超时后可读持久 written，同 ID 返回原结果且不再次写入 |
| 输出预算 | `TestModelProjectionFitsToolResultBudget` | 两路最大输出经转义与 JSON 编码后仍在工具结果限额内 |

模型为本地夹具，进程、tmux 和 IPC 为真实实现。普通 CLI 仍待真实 provider 接入；本节不作为真实模型兼容性结论。

## 2026-10-15：文本 patch

4.3 已完成，累计 18/68 项。全量普通测试和 race 通过；解析器 fuzz 在 5 秒预算内完成 37915 次输入检查并通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 四种操作与非 Git 目录 | `TestAllOperationsWithoutGitPreserveTextAndMode` | 新增、删除、更新、移动、空文件、移动并编辑通过；保留换行与可执行位，返回哈希和行范围 |
| 全部目标预校验 | `TestEveryConflictIsCheckedBeforeAnyWrite` | 旧内容不符、目标已存在、重复/父子目标、非文本/特殊文件均在写入前拒绝，先列出的新目录也未创建 |
| 快照与并发 | `TestPreflightSnapshotAndPathChangesAreRechecked`、`TestStagingRechecksConcurrentEditsAndNoClobberPublish`、`TestConcurrentPatchesSerializeAndCancellationPreservesFiles` | 内容、inode、路径解析变化被核实；新增发布不覆盖竞争目标；同 core 调用串行，取消保留已有文件 |
| 应用阶段 I/O 故障 | `TestIOFailureReportsAppliedRangeAndMoveHalf` | 第二个文件失败保留第一个结果；移动删除失败保留两份并报告，目录同步失败标未知；已创建目录有记录 |
| 文本及限额 | `TestPatchHunksMatchExactlyAndPreserveLineEndings`、`TestPatchLimitsAndSpecialFilesFailBeforeWriting`、`TestPatchResultFitsModelBudget` | 精确/唯一/EOF 匹配、LF/CRLF/无尾换行、文件和路径限额，以及结果可保存性通过 |
| 模型与命令共用 | `TestLoopPatchConflictRecoveryAndShellGenerator` | 五次夹具模型请求、四个真实工具调用；冲突回填后修正，命令生成器正常写文件，派发前有持久意图 |
| 不完整调用 | `TestIncompletePatchCallNeverWrites` | 参数断流不应用 patch，目标文件不存在 |

本节覆盖 V08；模型仍为确定性夹具。并发保证针对同一 core 的 patch，外部写入竞争窗口、部分应用和持久性边界见 [文本 patch](./patch.md)。

## 2026-10-15：输出分类与有界修复

4.4 已完成，累计 19/68 项。全量普通测试、race 通过。分片一致性 fuzz 在 5 秒预算内检查 34696 个输入并通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 显式标记与字面量 | `TestExplicitOutputAcrossEveryByteBoundaryAndLiterals`、`TestInvalidMarkedOutputNeverBecomesFinal` | 每个字节边界、中文、LF/CRLF、引用、转义、不同长度围栏通过；缺失闭合、嵌套、重复 final 和块外正文报错 |
| 原生类别与私有数据 | `TestNativeCategoriesAreExplicitAndPrivateDataIsUnchanged` | 已声明原生类别直接分类，未声明能力不信任裸类别；原文和不透明字段保持原样 |
| 持久原文与分类 | `TestLoopTaggedByteFragmentsKeepRawContextAndProjection` | 原始标签继续在上下文中；跨 UTF-8 字节通过 `text_bytes` 保存，分类投影与原文可分别查询 |
| final 与工具冲突 | `TestFinalConflictSettlesToolsThenRepairsWithoutReplay` | 两条分类路径都先保存原调用结果；修复期间换 ID 请求工具仍被拒绝，副作用仅一次 |
| 修复上限 | `TestFormatRepairBudgetAndRawMessagesAreAppendOnly` | 0、1、2 次配置严格生效，次数/上限可查询，原用户输入与全部无效回答保留 |
| 断流与未知结果 | `TestInterruptedFinalRepairDoesNotReplayPriorOrPartialTools`、`TestUnknownToolResultStopsBeforeFormatRepair` | 已完成调用不重跑，不完整调用不执行；unknown 副作用阻止进入修复 |
| 继续工作与显示边界 | `TestValidCommentaryCanContinueAfterTextOnlyRepair`、`TestLiteralToolResultDoesNotControlOutputCategory`、`TestClosedFinalWaitsForWholeResponse` | 有效 commentary 后恢复工具；用户/工具伪标签不影响分类；final 闭合仍等完整响应结束 |
| 事务与限额 | `TestFormatRepairInstructionAndCounterCommitTogether`、`TestOutputBatchingAndLimits` | 修复事件和提醒原子提交；同片段正文合并，分类展开有上限 |

本节覆盖 V15 的公共分类与修复行为。原生类别目前由测试 adapter 声明；各协议字段和真实端点仍由后续 adapter 任务验证。文档按 trim-cn-tech-doc 保留次数、错误和恢复条件。

## 2026-10-15：Responses HTTP adapter

4.5 已完成，累计 20/68 项。全量 `go test ./...`、`go test -race ./...` 与 OpenSpec strict 校验通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 工具流与私有字段 | `TestHTTPInterleavedCallsPrivateReplayAndLatePhase` | 交错参数按调用身份组装；reasoning、加密字段、完整 function/message、annotations 和 phase 原样回传；缺失用量保留未知 |
| SSE 与内容顺序 | `TestSSEFragmentationLineEndingsAndContentParts`、`FuzzSSEChunking` | 单字节分片、CR/LF/CRLF、BOM、注释、多行 data 和多个 content part 顺序通过；5 秒 fuzz 预算检查 23831 个输入通过 |
| 错误与取消 | `TestHTTPFailureCancellationAndRedaction`、`TestMalformedStreamsNeverAuthorizeCalls` | HTTP 错误脱敏、重定向拒绝、取消关闭连接；参数不完整、身份变化、重复字段、完成内容不符均报错 |
| 分类与限制 | `TestLateNativeCategoryWaitsWithoutGuessing`、`TestDoneSuffixAndAbsentPhaseRemainExplicit`、`TestSSEAndAssemblyLimits` | phase 晚到前不发布未分类正文；无类别保留原文并报格式错误；SSE/组装超限拒绝 |
| 配置与凭据 | `TestConfiguredBindingAndLateCredentialResolution`、`TestRequestRejectsLostContentAndInvalidCalls` | 配置快照固定，凭据只在 HTTP 请求时解析；未配置型号、异端点私有数据、附件与不完整历史明确拒绝 |
| 持久循环 | `TestResponsesLoopTwoRoundsErrorAndPersistedPrivateItems`、`TestResponsesPartialArgumentsNeverExecute` | 原生/标签两条路径均完成两轮工具和错误回填；原始私有 item 进入 SQLite 上下文；断流参数只保存、不执行 |
| 真实 CLI 接线 | `TestConfiguredResponsesBinaryExecutesPatch` | 独立临时 XDG 下构建并启动真实 core，读取 Responses 配置，经 HTTP 夹具调用真实 patch，回填后保存 final；历史不含测试 key |
| 参数续接 | `TestJSONContinuationSurvivesStorageWithoutLosingIntegers` | JSON 落盘转义 HTML 不影响续接比较；大整数差异不丢失，真实 CLI 的 HTML patch 用例覆盖下一轮回传 |

这些测试使用本地 HTTP 夹具，未访问真实模型服务。MCP 原生搜索、附件、跨协议迁移、上下文压缩及真实端点能力由后续任务验收。

## 2026-10-15：停止、人工暂停与恢复

4.6 已完成，累计 21/68 项。全量 `go test ./...`、`go test -race ./...` 与 OpenSpec strict 校验通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 模型取消与续接 | `TestStopModelStreamPersistsPauseAndResumeUsesNewTask` | 取消请求中断模型流，已收片段保留；暂停持久化，resume 使用新 Task，旧 cancelled 不变，不触发格式修复 |
| 调用尚未结束 | `TestCancellingWaitsForToolAndPreservesEffectsSkipsRemainingCalls` | 工具忽略取消时保持 cancelling；真实文件效果保留，结果落盘；剩余调用补上未执行结果，提前 resume 等待旧调用落定 |
| 后台进程 | `TestStopWhileWaitingKeepsAcceptedBackgroundProcess` | 真实 pipe/tmux PTY 在 Agent 停止后继续，保存 process ID 和 observation_interrupted；继续输入后原进程完成，命令效果仅一次 |
| 未知效果 | `TestUnknownToolCancellationHoldsAcceptedResume`、`TestUnknownCancellationCannotBeResumedOrBypassed` | Task 保持 cancelling，Agent 为 unknown；已接受的恢复、新输入和新操作 ID 均不启动新循环 |
| 取消与错误 | `TestCancellationDoesNotHideObservationCorruption` | 主动取消读取保留最后观察结果；同时存在的记录损坏不被取消掩盖 |
| 调度及去重 | `TestStopResumePreserveTerminalHistoryAndDedup`、`TestExplicitInputPrecedesQueueAndResumeDuringCancellationWaits` | 停止后无新模型/工具派发；新输入优先，旧队列随后；重试旧 stop 不取消后来任务，resume 不改旧终态 |
| final 与作用范围 | `TestStopAndFinalRaceHasOneTerminalAndPersistentPause`、`TestStoppingOneAgentDoesNotCancelAnother` | 30 组并发 stop/final 每组只有一个终态并保留暂停；停止一个 Agent 不取消另一 Agent；既有 final 判定测试继续通过 |
| 原子性与恢复 | `TestStopAndResumeTransactionsRollBackCompletely`、`TestIdlePauseResumeCreatesNewTaskAndUnknownCrashKeepsPause` | 注入事件写入失败时状态、任务和接受记录一起回滚；取消中崩溃保留暂停及待核实状态 |
| 迁移与 CLI | `TestSchemaOneMigrationPreservesHistoryAndAcceptance`、`TestCLIJSONStdioAndStableRetry` | schema 1→2 保留原任务、历史、接受记录及数据库约束；stop/resume 经真实 Unix socket 去重，help 可查询 |

停止相关 runtime 用例连续运行 10 次通过。模型为确定性夹具，文件写入、进程、tmux、SQLite 与 IPC 为真实实现。子 Agent/mailbox 及树级停止由第 6 组验收，本节不作为完整 V06 的通过记录。

## 2026-10-15：基础崩溃续接

4.7 已完成，累计 22/68 项。派发前保存标记，启动时核实原调用，已知结果提交后继续原 Task；未知效果保持等待。全量普通测试和 race 通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 派发前记录 | `TestDispatchMarkerPrecedesExecutor` | 执行器运行前可查到原调用的准备和派发记录 |
| 中断点与只读查询 | `TestRecoveryJournalDispatchBoundariesAreReadOnly` | 片段、计划、准备、派发、未知、旧版意图、跳过、结果落盘及 final 分别识别；重复查询不改状态和 cursor |
| 身份与上下文 | `TestRecoveryJournalRejectsChangedIdentityAndIncompleteResults`、`TestRecoveryJournalRestoresRepairBudgetAndCancellation` | 换操作 ID、参数、调用 ID、版本及未配对结果被拒绝；保留修复次数和取消状态 |
| 查询入口 | `TestRecoveryJournalSelectsExplicitTaskAndRejectsCrossAgent`、`TestCLIJSONStdioAndStableRetry` | 默认/指定 Task、跨 Agent 拒绝、真实 Unix socket 查询及 help 通过 |
| 进程核实 | `TestRecoverProcessAndInputWithoutReplayingEffects`、`TestRecoverInputRequiresOriginalCompletedControl` | 真实 pipe/PTY 及输入效果保持一次；恢复路径禁止 Start/Input；原控制记录证明 written、部分失败，查询失败保留最后观察状态 |
| 证据缺失 | `TestRecoverExecRejectsMissingUncertainAndDifferentProcess`、`TestRecoverReadOnlyCallNeverNeedsControlRecord` | 缺失、unknown/lost、启动参数不符保持待核实；只读调用无需输入记录 |
| PTY 退出 | `TestZombieWaitStatusRequiresOriginalIdentity`、`TestRecoverPaneExitFromOwnedZombie` | Linux 原 zombie 的退出码 7 可核实；错 PID、父进程、启动身份、pane、token 与命令均拒绝 |
| 原任务续接 | `TestRecoveryContinuesOriginalTaskAtDispatchBoundaries`、`TestRecoveryIncompleteStreamAndSavedFinal` | planned/prepared/dispatched/completed 四处重开数据库，命令效果一次；原 Task 和输入一份，未完成片段不拼入新响应，已保存 final 直接完成 |
| 运行中接回 | `TestRecoveryRejoinsRunningPipeAndPTY` | 真实 pipe/PTY 接回原 process ID，继续输入后退出；不重新启动命令 |
| 显式核实与停止 | `TestExplicitRecoveryDoesNotTreatNewInputAsProof`、`TestRecoveryCancellationSkipsPreparedCallsWithoutModel` | 新输入不触发核实或执行；显式 recover 核实原效果后处理队列；停止中的未派发调用补取消结果，无需模型 |
| 原设置与凭据 | `TestBindingRestoresOriginalEndpointModelAndCredentialReference` | 当前连接已移除时仍恢复原端点/模型/力度；凭据引用延迟解析，可读取轮换后的值；快照不含 key 或无关型号 |
| 修复额度 | `TestRecoveryDoesNotResetInterruptedRepairBudget`、`TestRecoveryUsesAlreadyReservedRepairWithoutChargingAgain` | 已派发修复计入原上限；已预留但未请求的次数继续使用，不重复收费；修改配置不刷新旧额度 |
| 核实事务 | `TestRecoveryAcceptanceAndActivationPreserveOriginalTask`、`TestVerifiedResultAndActivationRollback` | recover 接受记录去重；结果、上下文和激活故障完整回滚；过期游标拒绝，Task 和输入身份不变 |
| patch 独立结果 | `TestPatchReceiptRecoversAndDeduplicatesWithoutEditing`、`TestPatchReceiptFailureBoundariesNeverReplay` | 并发同 ID 编辑一次；恢复返回历史结果，保留后来的外部修改；意图失败、编辑后中断和结果写入失败均不重放 |
| patch 待核实 | `TestPatchReceiptRejectsChangedIdentityAndCorruption`、`TestRecoveryPatchReceiptContinuesWithoutReapplying` | 参数/身份变化、损坏、版本不符或无记录继续等待；新输入不绕过；已知成功/冲突回填原 Task |
| 真实 core 故障 | `TestBinaryRecoversOriginalRunningToolAfterCoreSIGKILL` | 真实 CLI/HTTP 夹具启动长命令，效果发生后 SIGKILL core；移除临时连接配置，再启动仍恢复原模型绑定与进程，继续输入后完成；一份 Task、三次模型请求、效果一次 |

回归中复现 tmux pane 已关闭而退出字段为空、原 shell 仍为 zombie 的情况。修复后 `TestSignalCancelAndOwnedGroups/true` 连续 30 次通过。新增调度器恢复检查还复现了关闭期间返回 `context canceled` 的时序问题；修复后 `TestUnknownToolCancellationHoldsAcceptedResume` 在 race 下连续 100 次通过。原失败保留为问题记录，不计通过。

进程、SQLite、文件和 CLI 为真实实现；模型只使用本地夹具。缺失/不完整 patch 记录仍保持待核实，文件当前内容不作为原执行完成的证明。mailbox、插件和压缩后的恢复由第 9 组扩展。

## 2026-10-15：基础工具与恢复操作说明

4.8 已完成，累计 23/68 项。[本地开发](./development.md)、[进程](./processes.md)、[patch](./patch.md) 和 [提示词](./prompts.md) 对应已注册能力；按 trim-cn-tech-doc 合并重复解释，保留参数、限额、失败与恢复条件。

文档中的两条 CLI 链路测试重新运行通过：`TestConfiguredResponsesBinaryExecutesPatch` 执行 patch、回填结果并保存 final；`TestBinaryRecoversOriginalRunningToolAfterCoreSIGKILL` 验证执行、续写/读取、CLI 查询和原任务恢复。全部使用隔离临时配置；未使用真实模型凭据，也未进行用户级安装。

## 2026-10-15：Alt-screen 客户端生命周期

5.1 已完成，累计 24/68 项。使用 Bubble Tea 2.1.0、Bubbles 2.2.1 和 Lip Gloss 2.0.6；按 tui-design 将网络操作放入 Cmd，并使用框架管理终端恢复。布局保持对话、输入和状态三部分。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 异步边界 | `TestUpdateAndViewNeverRunCoreIO` | 阻止同步 I/O 的客户端下，Init/Update/View 正常返回；状态刷新合并，粘贴不提交，Ctrl+C 只退出 |
| 请求身份与草稿 | `TestPendingMutationRetainsIdentityAndNewDraft`、`TestDetachAndCommandsDoNotStopWorkImplicitly` | 接收未知时保留原请求；重试参数/ID 不变，成功回调保留新草稿；只有显式 stop 才发送停止 |
| 尺寸与控制字节 | `TestShellSizesAndLiteralControlText` | 120×40、80×24、60×24、48×16 和过小窗口通过；中文/组合字符/emoji 不超宽，ESC/OSC 不执行 |
| 真实终端恢复 | `TestRealTerminalRestoresModesOnDetachCancellationAndPanic` | Linux PTY 中正常退出、取消、注入 UI panic 后退出 alt screen、恢复原 termios，订阅结束 |
| 后台独立 | `TestRealTerminalDetachLeavesCoreTaskAlive` | TUI 经真实 Unix socket 附着；Ctrl+C 后原 Task 仍 running，释放模型夹具后同一 Task 完成 |
| 线性输出 | `TestCLIJSONStdioAndStableRetry`、`TestHelpVersionAndUsage` | 非 TTY 和 `--plain` 返回只读 JSON 快照，无 ANSI；TUI 帮助及既有 CLI 行为通过 |

全量普通测试与 race 通过。输入控件、终端和 IPC 为真实实现，模型仍为本地夹具；完整显示、重连与交互验收保持后续任务未完成。

## 2026-10-15：事件展示与自动重连

5.2、5.3 已完成，累计 26/68 项。全量 `go test ./...`、`go test -race ./...` 通过；真实终端生命周期、重连及输入关闭测试在 race 下连续 10 次通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| CLI/TUI 同源 | `TestCLIHistoryMatchesTUIProjection`、`TestHistoryProjectionAndFoldPreserveFacts` | 同一事件夹具经过 CLI JSON 和 TUI 投影，Agent/Task/请求/调用/进程身份、正文与终态一致；折叠和重复事件不改变记录 |
| 分类与私有数据 | `TestInvalidInterruptedAndPrivateOutputs`、`TestToolRecoveryProjectionAndSplitRawUTF8` | provisional final 保持未确认，无效/中断状态可见；迟到分类不重复正文，私有续接与 developer 消息不进入显示；原始 UTF-8 字节可重组 |
| 内存与缺口 | `TestProjectionBoundsAndInvalidEventCursor`、`TestGapCannotBeHiddenBySnapshotOrStaleWatch` | 条数/正文缓存有界且注明裁切，非法事件不推进游标；新快照和旧连接不能掩盖缺口 |
| 重连与草稿 | `TestReconnectKeepsCursorDraftAndPendingIdentity`、`TestInitialFailureRetriesAndTimerCancels` | 初次离线也重试；旧订阅消息失效，空闲连接可确认在线，草稿和未确认请求 ID 保留，退出取消退避计时 |
| 真实终端断线 | `TestRealTerminalReconnectAfterOfflineTaskCompletion` | 停止 Unix socket 服务期间原 core Task 完成；重开服务后 TUI 从原 seq 补齐终态，草稿保留；TUI 发出的写请求为零，Task 仍只有原来一份 |
| 输入关闭竞态 | `TestInputReaderConcurrentCloseJoinsRead`、`TestRealTerminalRestoresModesOnDetachCancellationAndPanic` | 关闭先唤醒并等待读取；并发重复 Cancel/Close、正常退出、取消及 panic 均通过 race，原 termios 恢复 |

全量检查曾复现两项失败：原始终端输出的连续字符串断言受增量刷新影响，已改为同时检查核心身份、界面状态和真实终端输出；Bubble Tea 强制关闭时，`cancelreader v0.2.2` 的 Close 与 Read 竞争描述符，已通过带 MIT 许可证的项目内补丁修复。补丁来源和范围见 `third_party/cancelreader/ZENDE.md`；上游自带测试也通过。共享模块缓存和参考仓库未修改，原失败不计为通过。

TUI 按 tui-design 保留三行固定输入/状态区，事件格式化为纯读取，网络和退避放在 Cmd。使用说明按 trim-cn-tech-doc 合并重复解释，保留游标、错误、缓存限制和请求去重条件。模型仍为本地夹具，未进行真实模型或用户级安装验收。

## 2026-10-15：PTY 选择、日志与接管

5.4 已完成，累计 27/68 项。全量普通测试、race 及两个本地补丁依赖的自带测试通过；真实接管与输入读取器测试在 race 下连续 10 次通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 进程目录 | `internal/process/list_test.go`、`TestProcessCLIControlAndBinaryInput` | Agent 筛选、稳定分页、帧预算、CLI list/attach 帮助和非 TTY 拒绝通过 |
| 定向控制 | `TestProcessSelectionAndResizeNeverBroadcast`、`TestReadOnlyLogsPipeAndUncertainResize` | 窗口只调整选定 PTY，突发 resize 合并；pipe/只读视图不调整，未知结果保留原 ID |
| 日志与布局 | `TestLogBytesAreBoundedLiteralAndKeepGaps`、`TestProcessPanelFramesPreserveTargetAndFit`、`TestLogListSelectionStaysReadOnlyAcrossPages` | ESC/OSC 转义、64 KiB 缓存、缺口、目标标题及窄屏边界通过；日志列表翻页后仍只读 |
| 真实接管 | `TestRealTerminalHandoffTargetsOnePTYAndReplaysEvents` | 两 PTY 和一 pipe 中只调整目标；原 PTY 接收输入，模型在接管期间完成；返回补齐原任务终态，退出恢复 termios，原进程继续 |
| 订阅与描述符 | `TestHandoffCancelsOnlySubscriptionAndReplaysOriginalCursor`、`TestInputCancelReleasesHandoffDescriptors` | 接管只取消订阅，返回使用原 seq；被替换的输入读取器释放 epoll/pipe，重复关闭安全 |

重复 race 曾复现 Bubble Tea 2.1.0 的 `checkResize` 在 SIGWINCH 与 `RestoreTerminal` 间并发写启动尺寸。项目内补丁改为仅投递尺寸消息，renderer/model 由事件循环更新。另修正测试清理：先等待自建采集器退出，再清理临时目录。`cancelreader` 补丁也在 Cancel 时释放已失效读取器。来源与范围保存在 `third_party/*/ZENDE.md`，模块缓存与参考仓库未修改。

```sh
go test -race ./internal/render -run 'TestRealTerminalHandoff|TestInputReader|TestInputCancel' -count=10 -timeout=90s
go test -race charm.land/bubbletea/v2 github.com/muesli/cancelreader
```

## 2026-10-15：TUI 到 core 恢复的纵向链路

5.5 已完成，累计 28/68 项。`TestBinaryRecoversOriginalRunningToolAfterCoreSIGKILL` 增加真实 CLI TUI 的 detach/重新打开，并分别覆盖 pipe、tmux PTY。普通测试通过，宿主测试在 race 下连续 3 次通过；子二进制为常规构建，TUI 内部并发另由 render 的 race 用例覆盖。全量普通测试、race 和 CLI 帮助/版本检查通过。

两个场景均核实：原命令已产生效果；TUI detach 不改 Task；第二个 TUI 接回原历史；core 被 SIGKILL 后显示离线，runner 保留原进程身份；移除当前模型配置后重启 core，保存的绑定继续工作；原 Task 完成，TUI 自动显示 final。两次退出均恢复 termios，Task 一份、文件效果一次、HTTP 模型请求三次。

```sh
go test ./internal/cli -run TestBinaryRecoversOriginalRunningToolAfterCoreSIGKILL -count=1 -v
go test -race ./internal/cli -run TestBinaryRecoversOriginalRunningToolAfterCoreSIGKILL -count=3 -v -timeout=120s
```

`-v` 输出各阶段及 Agent/Task/process 身份。数据、配置、socket、终端与清理目标均属于测试临时目录；模型为本地 HTTP 夹具，未使用真实凭据，未安装用户二进制。操作及失败边界见 [本地 CLI 链路验证](./development.md#本地-cli-链路验证)。

## 2026-10-15：极简绿色界面与按需状态

5.6 已完成，累计 29/68 项。全量 `go test ./...`、`go test -race ./...`、`tui --help` 和 `agent activity --help` 通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 固定布局与配色 | `TestMinimalFramesThemeAndSymbolFallback` | 80×24、60×24、48×16 × 四种颜色能力 × 两种符号模式，共 24 组逐行检查；中文、组合字符、emoji、光标位置及三行输入/状态区通过，空闲无后台/警告占位 |
| 窄屏与折叠 | `TestBusyNarrowFrameKeepsInputStatusAndErrorBudget`、`TestActivityNarrowRowsKeepStateAndOriginalIdentity` | 长 cwd/型号/错误不挤掉输入或状态；后台行保留终态文字，commentary 折叠保留原文 |
| 错误去重 | `TestIssuesFirstOccurrenceOnlyAndBoundedMemory` | 首次展开、重复收起、命令展开、Esc、解决后移除入口及 64 类缓存通过；主动展开的详情不被重复刷新关闭 |
| 组合输入与模型 | `TestComposerDraftsAndPasteAreIndependent`、`TestDefaultModelStatusComesFromSavedPublicBinding` | 两个输入实例草稿独立，粘贴/主题/resize 保留中文多行；默认型号读取公开绑定字段，私有快照不显示 |
| 核心后台查询 | `TestActivityScopesTreeAndKeepsHistoryReadOnly`、`TestActivityIsBoundedAndExcludesFinishedTasks`、`TestActivityObservesProcessesWithoutControllingThem` | 当前 Agent 树筛选、循环祖先终止、1000 项截断、已结束项排除、进程分页及不可用来源通过，查询不写历史、不控制进程 |
| 计数与真实终端 | `TestActivityCountIsConditionalAndPollingIsCoalesced`、`TestBinaryRecoversOriginalRunningToolAfterCoreSIGKILL` | 空闲隐藏、未知不变零、刷新不重叠、旧计时器失效；真实 pipe/PTY 在 CLI activity 中出现并于完成后消失，真实 TUI 使用 NO_COLOR/ASCII 并恢复 termios |

固定帧测试发现重复的空白输入提示符和误用左裁切参数导致 cwd 丢失，修正后通过。按 tui-design 保留零边框、两行输入加一行状态；主题取自原型，16 色模式沿用默认前景以避开被改成粉色的 ANSI green。说明按 trim-cn-tech-doc 合并重复表述，保留查询、错误、刷新及显示限额。

本节覆盖 V19/V20 的主题、布局和折叠部分；命令候选、点击共用入口、模型提问与图片仍由 5.7 验收。模型仍为本地夹具，未进行用户级部署。

## 2026-10-15：命令入口、候选与草稿

5.7 部分实现，完成数仍为 29/68。命令、候选、点击和进程列表快捷键共用 registry；点击目标与绘制使用同一份单元格布局。全量 `go test ./...`、`go test -race ./...` 通过；真实 PTY 候选流程及相关状态测试在 race 下连续 10 次通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 入口与参数 | `TestCommandRegistryOwnsHelpAliasesAndValidation`、`TestCommandQuotingIsLiteralAndRejectsIncompleteInput` | 帮助由 registry 生成，别名一致；多余参数和未闭合引号保留输入并拒绝执行，参数不作 shell 展开 |
| 下方候选 | `TestPaletteBelowComposerAndStatusStaysLast` | 80×24、60×24、48×16 下候选最多四行，避开两行输入，状态保留底部；所有命令可用方向键到达 |
| 草稿与输入 | `TestInlineCommandAndTabCompletionPreserveDraft`、`TestPasteIMEAndCaretDoNotExecuteCandidates`、`TestControlCommandAcceptanceNeverClearsRestoredDraft` | 中文多行草稿在临时命令、返回、参数错误与未知请求重试后保留；Tab 只补全，粘贴与 IME 已提交文本不自动发送，原请求身份保持 |
| 点击与逐条折叠 | `TestEveryClickTargetHasRegistryRouteAndKeepsDraft`、`TestCommentaryHitIdentitySurvivesWrapScrollAndFold`、`TestCommentaryEntrySearchProvidesKeyboardAccessWithoutOpaqueIDs` | 后台、警告、离线刷新和 commentary 点击与命令一致；换行/滚动后的目标身份正确，正文搜索可选单条，折叠不改历史 |
| 真实终端 | `TestRealTerminalCommandPalettePreservesMultilineDraft` | Linux PTY 经 bracketed paste 输入中文多行，再用 Tab/Enter 打开和返回；零写请求，Ctrl+C 退出恢复 alt screen、鼠标模式及原 termios |

```sh
go test -race ./internal/render -run 'TestRealTerminalCommandPalette|TestCommentaryEntrySearch|TestControlCommand' -count=10 -timeout=60s
```

按 tui-design 将候选放在输入下方，按 trim-cn-tech-doc 合并操作说明，保留参数、限额与错误处理。终端只提供已提交的输入字符；这里验证了中文提交和粘贴路径，未声称检查系统输入法内部的组合状态。

## 2026-10-15：图片持久输入与 CLI

5.7 继续实施，完成数仍为 29/68。新增附件导入/查询/读取接口、CLI `attachment` 与 `agent submit --image`。原始图片与元数据同事务保存，任务和上下文仅引用附件；选择支持图片的 Responses 型号后，原字节进入模型请求。全量普通测试和 race 通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 格式与限额 | `TestInspectVerifiesBytesAndBoundsBeforeDecoding`、`TestImageTransportHasExplicitCapabilityAndByteBudget` | PNG/JPEG 按字节识别，完整解码校验；字节/像素超限拒绝，重复引用计入请求预算；缺能力在凭据解析和网络调用之前报错 |
| 持久化与迁移 | `TestImagesRetainBytesReferencesAndRetryAcrossRestart`、`TestImageImportTransactionRollbackAndReferenceValidation`、`TestSchemaTwoMigrationKeepsExistingInputsAndPause` | 重开数据库后字节、引用和接受记录一致；操作记录写入失败时整个导入回滚；禁止改删附件/任务输入，跨 Agent 和重复引用拒绝，列表有界分页，schema 2→3 保留输入、暂停和优先级 |
| 核心与协议 | `TestImageImportAndCapabilityErrorsKeepOriginalBytesAndInput`、`TestResponsesImageBytesSurviveToolRoundAndSourceReplacement`、`TestBindingRestoresOriginalEndpointModelAndCredentialReference` | 源文件删除后导入重试返回原记录；不支持图片时零新任务，原附件可读；两轮 HTTP 请求都携带原字节，普通历史不展开图片；恢复保留型号的图片能力声明 |
| CLI | `TestAttachmentCLIImportRetryReadAndImageOnlyInput` | 经真实 Unix socket 导入、重试、分页、读取和纯图片提交；stdout JSON 稳定，中文文件名与命令帮助通过 |

CLI 用例首次因长测试名导致临时 Unix socket 路径超限，改用独立短临时目录后通过；核心路径长度校验保持不变。模型为本地 HTTP 夹具，未使用真实凭据或安装用户二进制。按 trim-cn-tech-doc 合并操作与限制说明，保留字段、预算和故障条件。

## 2026-10-15：TUI 图片草稿与键盘操作

5.7 的图片输入已接通，完成数仍为 29/68。`/image` 导入、`/images` 分页、`/image-use` 选择及 `/image-remove` 移除共用命令入口；图片行按需显示，候选仍在输入下方。全量普通测试和 race 通过，图片相关测试在 race 下连续 5 次通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 草稿与回执 | `TestComposerImagesAreIndependentAndCommandsOnlyConsumeText`、`TestImageOnlySubmitAndLateAcceptanceKeepNewDraft`、`TestImageImportRetryUsesOriginalOperationAndDraft` | 中文多行、附件和临时命令输入独立；纯图片可提交；编辑后恢复相同文字、新增附件、导入中及新草稿均不被旧回执清除；未知导入重用原请求，不支持图片时保留输入 |
| 目录与点击 | `TestImageCatalogGenerationCandidatesAndReadOnlySelection`、`TestImageCatalogRejectsInvalidPagesAndStaleClosedPanel`、`TestImageClickAndCommandHaveSameEffectWithoutConsumingDraft` | 分页和异步候选更新通过；错误身份、重复项、超限页和旧回复拒绝；点击与命令行为一致，选择只读元数据，移除只改草稿 |
| 布局与历史 | `TestImagesLayoutIsConditionalBoundedAndKeyboardReachable`、`TestTranscriptKeepsBoundedImageReferencesWithoutImageBytes` | 80×24、60×24、48×16 与三种颜色能力通过；图片行、候选、光标和状态不重叠，文件名控制符可见转义；纯图片历史显示数量，引用计入缓存预算 |
| 真实终端与 core | `TestRealTerminalImagesThroughCorePreserveRejectedDraft` | 48×16 Linux PTY 经 bracketed paste 输入中文、多行和含空格路径；Unix socket 完成导入、能力拒绝、保留后重发及重新选择；源文件删除后原字节仍进入模型，只有一个任务；退出恢复 alt screen、鼠标模式和 termios |

```sh
go test -race ./internal/render -run 'Test(ComposerImages|Image|RealTerminalImages|TranscriptKeeps)' -count=5 -timeout=90s
```

边界测试发现文件名中的换行使 24 行界面扩为 27 行；显示层改为单行转义后通过，原元数据保持。终端与 core 为真实本地组件，模型使用 fixture，未使用外部凭据。按 tui-design 保留按需单行附件区，按 trim-cn-tech-doc 合并操作说明与失败条件。

## 2026-10-15：持久提问、CLI 与模型续接

5.7 增加 question 接口和模型 CLI 入口，完成数仍为 29/68。schema 4 保存问题、回答、图片引用和投递游标；模型在工具结果完成后等待，回答以 user 消息进入下一次请求。全量普通测试和 race 通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 原子保存与重试 | `TestQuestionAnswerDeliveryIsDurableOrderedAndIdempotent`、`TestQuestionTransactionsRollBackAcceptanceAndDelivery` | 重开数据库后接受记录和投递身份保持；故障注入覆盖提问接受、回答接受和上下文投递，部分写入回滚；旧回答及问题正文受数据库约束保护 |
| 限额与停止 | `TestQuestionsBoundInputScopeAndWaitForAllAnswers`、`TestQuestionConcurrentAnswerAndTaskStopKeepHistory`、`TestQuestionWaitStopsWithTaskWithoutAnotherModelRequest` | 8 KiB 问题、1 MiB 回答和 8 项待处理上限通过；跨 Agent、无图片能力和停止后的回答拒绝；并发重试只接受一次，停止取消未回答问题并保留已接受回答 |
| 循环与恢复 | `TestQuestionLoopWaitsAfterToolsAndReceivesTextAndImages`、`TestQuestionWaitingSurvivesRecoveryWithoutRepeatingTool` | 工具结果先于回答，文字和图片一起进入模型；等待时其他 Agent 可完成；恢复前后回答均保持一次投递，原 Task 与已完成工具不重放 |
| CLI 与提示词 | `TestQuestionCLIUsesDurableCoreAndStableRequests`、`TestQuestionPromptHintsMatchRegisteredCLIAndQuotePaths`、`TestQuestionCLIHintRequiresRegisteredExecTool` | 经真实 Unix socket 验证 stdin、文件、分页、重试、JSON 输出和错误通道；提示只列已注册命令，路径引用与 exec 能力条件通过 |
| 真实二进制 | `TestBinaryModelAsksThroughCLIAndContinuesWithAnswer` | 本地 HTTP 模型夹具调用真实 exec_command，经独立 runner 的 pipe 执行 CLI 提问；另一 CLI 回答后原 Task 完成；两个模型请求、一个工具派发、一次提问/回答/投递 |
| 迁移 | `TestSchemaThreeMigrationKeepsImagesAndTasks` | schema 3→4 保留旧任务输入、附件引用与原字节；既有 1→当前、2→当前迁移测试继续通过 |

提问相关测试及二进制链路在宿主 race 下连续 3 次通过：

```sh
go test -race ./internal/runtime ./internal/store ./internal/cli -run 'TestQuestion|TestSchemaThree|TestBinaryModelAsks' -count=3 -timeout=90s
```

二进制用例最初在清理阶段先取消父 context，core/runner 因此被杀；调整为先发送 SIGTERM、等待退出再取消 context 后通过。子二进制为常规构建，宿主及包内并发用 race 检查。提示词参考本地 Pi `7bc154ad082e2f0029c19117a16d22a504439476`，说明按 trim-cn-tech-doc 合并重复规则。尚未连接 TUI 提问面板，也未执行真实供应商或用户级部署验收。

## 2026-10-15：TUI 提问与独立回答草稿

5.7 的提问输入已接通，完成数仍为 29/68。`/questions`、`/answer` 与点击共用入口，候选在输入下方，状态计数按需出现。全量普通测试和 race 通过，提问 UI 及真实终端测试在 race 下连续 3 次通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 独立编辑器 | `TestQuestionDraftSwitchingSharesCommandsNotEditors`、`TestQuestionImageReceiptsFollowHiddenDraftOwners` | 主输入、多个回答、临时命令之间切换保留中文多行和图片；尺寸及主题恢复；迟到的图片归还原草稿 |
| 接受与关闭 | `TestQuestionAnswerAcceptanceCannotEraseOtherOrEditedDraft`、`TestQuestionRejectionsRetryAndClosedFormsKeepInput` | 问题/Task 身份不匹配保留原请求；重试不换身份；回执保留后续编辑、其他回答和主输入；图片能力拒绝及关闭问题均保留草稿，回答不变成新任务 |
| 快照与限制 | `TestQuestionSnapshotsAndReplayDoNotReopenOrStealFocus`、`TestQuestionEventsRefreshCandidatesWithoutExecutingThem`、`TestQuestionDraftLimitNeverEvictsUnsentAnswers` | 旧回放和快照不能重开问题；外部回答只更新状态；候选随事件刷新而不执行；16 份草稿达限报错，显式清除只影响目标草稿 |
| 布局与历史 | `TestQuestionLayoutClickCommandParityAndNarrowWrapping`、`TestQuestionTranscriptValidatesBeforeProjectingAndBoundsAnswers` | 80×24、60×24、48×16 和三种颜色能力通过；点击均有命令入口；正文换行及候选不超宽；回答去重、图片引用与正文计入显示预算 |
| 真实终端与 core | `TestRealTerminalQuestionReconnectAndImageAnswer` | 48×16 Linux PTY、UDS 和 SQLite 验证关闭后重开；键盘输入中文多行和图片路径，删除源图片后仍投递原字节；主草稿恢复，原 Task 完成；两个模型请求、一次提问/回答/投递/工具派发，退出恢复 alt screen、鼠标和 termios |

提问 UI 重复验证：

```sh
go test -race ./internal/render -run 'TestQuestion|TestRealTerminalQuestion' -count=3 -timeout=90s
```

首次单测失败来自夹具：textarea 内容宽度不含两列提示符，图片接受记录也需填 `state: accepted`，修正断言与夹具后通过。按 tui-design 复用输入框并保持异步 I/O；按 trim-cn-tech-doc 合并过时说明、保留限额和验证边界。模型与提问执行器为确定性夹具；真实终端用例验证已保存问题重开，未验证未提交草稿跨 TUI 进程恢复。真实供应商和用户级部署验收仍未执行。

## 2026-10-15：资源发布、版本保留与 CLI

5.7 所需资源后端已接入，完成数仍为 29/68；7.1、7.2、7.6、7.7 保持未完成。schema 5 保存不可变包及发布历史，CLI 支持 register/refresh/list/status/read。全量普通测试、race 和 OpenSpec strict 校验通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 解析与文件边界 | `TestInstructionFrontmatter`、`TestPackageContentIdentityAndOwnedBytes`、`TestPrepareCapturesFilesWithoutRunningScripts`、`TestPrepareRejectsNonFilesEscapingLinksAndLimits` | 中文、BOM/CRLF、元数据、可执行位和附件版本通过；非法 YAML/路径、FIFO、越界链接、深度及容量超限拒绝；脚本未执行 |
| 持久版本与作用域 | `TestResourceVersionsSurviveRestartAndStayAgentScoped` | 重开数据库后原正文和二进制附件可读；同版本不重复发布，另一个 Agent 只能读自己发布过的版本；数据库拒绝改删版本和发布历史 |
| 原子发布与竞争 | `TestResourcePublishRollsBackEveryProjection`、`TestResourceConcurrentPreparationCannotRevertPublication` | 五处写入故障均完整回滚；同一基线并发发布只有一项成功，旧准备不能回滚新指针 |
| 目录和迁移 | `TestResourceMetadataPaginationAndValidation`、`TestSchemaFourMigrationKeepsQuestionsAndImages` | 元数据查询、分页、大小写/中文匹配和非法参数拒绝通过；4→5 保留已有任务、回答和图片字节 |
| CLI 与提示词 | `TestResourceCLIUsesDurableVersionedCore`、`TestResourcePromptHintsMatchRegisteredCLI` | 真实 UDS/SQLite 验证相对中文路径、旧版读取、损坏刷新、源文件删除后重试及 profile 隔离；说明对应已注册命令及当前二进制/profile |

使用隔离临时目录和数据库，未安装外部 skill、执行包内脚本或访问模型服务。尚未验证目录自动监测、安装期间半写文件的稳定判定、模型请求级目录刷新和 TUI `$`，这些仍在后续实现范围。说明按 trim-cn-tech-doc 合并操作步骤，保留限额、来源版本和失败条件。

## 2026-10-15：Skill 选择与固定版本输入

5.7 已完成，累计 30/68 项。`$`、`/skill` 和点击共用命令入口，主输入与回答复用草稿组件。schema 6 保存用户选中的 skill 版本；正文在输入实际投递时以 user 消息追加。全量 `go test ./...`、`go test -race ./...` 通过，相关用例在 race 下连续 3 次通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 固定版本与迁移 | `TestSkillInputPinsPublishedVersionAcrossQueueAndRestart`、`TestSchemaFiveMigrationPreservesInputAndResources` | 排队、更新目录和重开后仍使用原正文及来源；5→6 保留旧输入、回答，新增引用受不可变约束保护 |
| 回答与回滚 | `TestSkillOnlyAnswerSurvivesRestartAndDeliveryRollback`、`TestSkillInputValidationAndAcceptanceRollback` | 显式用户选择允许 disable-model-invocation；纯 skill 回答可投递，失败完整回滚，重试只追加一次；跨 Agent、缺失版本、重复/超量及 prompt 冒充 skill 均拒绝 |
| 元数据目录 | `TestSkillCatalogMetadataOnlyGenerationPaginationAndBounds`、`TestSkillCatalogBehindEventCursorStopsAutomaticRetry` | 只读元数据；分页代次不一致重读，非法顺序/游标/版本拒绝；持续变化或落后事件最多自动尝试 3 次 |
| 草稿与布局 | `TestSkillPickerPreservesTextImagesAndSelectionUsesNoIO`、`TestSkillOnlyMainAndAnswerLateAcceptance`、`TestSkillLayoutRemainsBoundedAndEveryTargetUsesRegistry` | 选择只改原草稿，保留中文多行和图片；迟到回执不清除新编辑；80×24、60×24、48×16 与三种颜色能力下输入/候选/状态互不覆盖 |
| 刷新、粘贴与历史 | `TestSkillRefreshKeepsSelectedVersionAndIgnoresOldAttempt`、`TestSkillPickerLoadingEmptyEscapePasteAndLateReplies`、`TestSkillTranscriptReferencesAreBoundedAndValidated` | 目录更新不替换已选版本；旧回执不重开候选，空候选不误发送，粘贴保持文本；引用计入显示预算，非法事件不推进游标 |
| CLI | `TestResourceCLIUsesDurableVersionedCore`、`TestSkillFlagRequiresExactVersion` | Unix socket 提交任务/回答的确切版本；源文件删除后仍使用保存正文，非法 `--skill` 拒绝 |
| 真实终端 | `TestRealTerminalQuestionReconnectImageAndPinnedSkills` | 扩展原提问终端用例：48×16 PTY 中用键盘选择旧版，更新目录后提交仍用旧版；关闭/接回后回答选择新版并附图片，删除两种源文件后原 Task 完成，termios/alt screen/鼠标恢复 |

```sh
go test -race ./internal/render ./internal/store ./internal/cli -run 'Skill|SchemaFive|ResourceCLI|RealTerminalQuestion' -count=3 -timeout=90s
```

首轮新增测试修正了夹具用法：回答回执须带 Task ID，内联参数命令先 Tab，CLI 问题文本使用位置参数。PTY 夹具把 skill 放在独立目录，避免将测试 socket 当成包文件；特殊文件拒绝规则保持。按 tui-design 复用下方候选与按需选择行，按 trim-cn-tech-doc 合并操作说明，保留版本、限额和恢复条件。模型使用 fixture，未访问外部服务；自动发现、目录监测和模型请求级通知仍待第 7 组实现。

## 2026-10-15：模型目录与工作环境只读接口

5.8 部分实现，完成数仍为 30/68。模型目录、目录分页和 Git 状态经 core/CLI 提供；查询保持 Agent 设置、任务与历史不变。全量普通测试、race 和 OpenSpec strict 校验通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 配置目录 | `TestCatalogConfiguredConnectionsVersionsAndNoCredentials`、`TestCatalogQueryValidationCancellationAndFrameLimit` | 空配置、同厂商多连接、型号/力度分页、版本变化、独立副本与页大小限制通过；凭据解析器未调用，返回值不含端点或凭据引用；未实现协议明确标记 |
| 目录浏览 | `TestDirectoryPagesNavigationAndLiteralPaths`、`TestDirectoryPaginationAcrossReadBatches`、`TestDirectoryInvalidAndDisappearingPaths` | 中文、空格、隐藏目录、链接、根目录、跨读取批次排序和路径失效通过；特殊字符按字面处理，非法字节名称报告 skipped，cwd 保持 |
| 真实 Git | `TestGitRealRepositoryCountsAndReadOnlyQueries`、`TestGitDetachedLinkedWorktreeAndConflict` | 初始分支、普通/暂存修改、未跟踪目录、上游计数、detached worktree 和真实未合并索引通过；索引字节/mtime 保持，fsmonitor 未执行，环境变量不重定向查询，非 Git 返回 none |
| 错误与字节边界 | `TestGitQueryBoundsAndRedactedFailure`、`TestGitPorcelainNULParsing`、`FuzzGitPorcelain` | 缺 Git、超时、输出超限及格式损坏明确报错；诊断不泄漏，特殊文件名不冒充状态记录；5 秒 fuzz 预算检查 138821 个输入通过 |
| CLI 与二进制 | `TestWorkspaceCLIQueriesKeepSessionAndContextUnchanged`、`TestWorkspacePromptHintsMatchCLI`、`TestConfiguredResponsesBinaryExecutesPatch` | 真实 Unix socket 验证只读、分页、帮助与错误通道；真实二进制读取配置目录不发模型请求，随后仍完成原 patch 链路；版本 4 提示词含已注册查询命令 |

目录/Git 与 provider 相关测试在 race 下连续 5 次通过。首轮测试发现嵌入 `bytes.Buffer` 的 `ReadFrom` 绕过输出限额，改为私有 buffer 后，真实子进程超量输出被拒绝；另修正未合并索引及复用 JSON 解码对象的测试夹具。旧失败不计为通过。

按 trim-cn-tech-doc 合并查询说明，保留来源、限额和错误条件。所有文件、Git 仓库、服务和模型端点均为临时测试对象；模型使用 HTTP 夹具。TUI 选择页、确认后持久切换和请求边界应用仍待实现，本节不作为 V21 全项通过或用户级部署验收。

## 2026-10-15：持久选配与请求边界

5.8 的 core/CLI 选配已实现，完成数仍为 30/68。schema 7 保存期望设置；每次请求保存实际设置，旧调用按原快照执行和恢复。全量 `go test ./...`、`go test -race ./...` 和 OpenSpec strict 校验通过，选配相关用例在 race 下连续 3 次通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 持久化与竞争 | `TestSelectionDurableAtomicAndIdempotent`、`TestSelectionRollbackAndHeldStates`、`TestConcurrentSelectionHasOneWinner` | 重开、旧请求重试、版本冲突及接受失败回滚通过；暂停/unknown 保持，复制中或归档拒绝；同一版本并发只有一项成功 |
| 请求事务 | `TestRequestBoundaryFencesSelectionContextAndStop` | 选择或上下文变化拒绝旧准备；notice 与 model.started 一起保存、一起回滚；stop 阻止新派发 |
| 模型预检 | `TestExplicitSelectionEffortAndCatalogFence`、`TestSelectionPreflightAllowsPendingCallsButRequestDoesNot`、`TestSelectionRejectsIncompatibleHistoryWithoutChangingIt` | 显式空力度不被 profile 默认覆盖；预检不解析凭据，允许未完成工具，实际请求拒绝；私有数据不兼容时保留设置与历史 |
| 循环与恢复 | `TestSelectionTakesEffectAfterOldResponseAndCalls`、`TestSelectionRecoveryUsesDispatchedRequestSnapshot`、`TestRequestSettingsJournalRejectsUnversionedChanges` | 流中切换不改旧工具 cwd；下一请求和重启恢复按对应快照；无版本变更的设置篡改被拒绝，旧上下文保留 |
| 默认值与图片 | `TestExplicitSelectionRunsWithoutProfileDefault`、`TestQuestionImagesUseLastDispatchedSelection`、`TestSchemaSixMigrationPreservesHistory` | 无默认模型时保留 accepted，选配后继续；回答按最新已发请求检查图片能力；6→7 保留旧输入和历史 |
| CLI 与真实二进制 | `TestSelectionCLIUsesPersistedAcceptance`、`TestSelectionPromptHintsMatchCLI`、`TestBinaryModelSelectsThroughCLIAtRequestBoundary` | 真实 UDS 验证选配、重试及错误通道；模型夹具经 exec 调用 CLI，原命令在旧目录产生效果，下一请求用新力度并在新目录执行 patch；三个请求、两个派发、一个 Task |

提示词版本 5 按 Pi 的能力组合方式提供选配入口；基础提示词允许后续 developer 通知更新工作目录和分类格式，通知复用同一分类规则。说明按 trim-cn-tech-doc 合并生效与恢复条件，保留参数、限额和错误。首轮测试将等待状态误写为 queued，按实际 accepted 状态修正；旧失败不计通过。

测试使用临时目录、SQLite、runner 与本地 HTTP 模型夹具，未访问外部模型。TUI 选择页与 Git 刷新仍待接入；跨模型私有数据迁移归 10.6，本节不作为 V21 全项或部署验收通过。

## 2026-10-16：TUI 选配与 Git 状态

5.8 已完成，累计 31/68 项。选择页复用持久选配接口，确认前只读；底部模型、cwd、Git 均有对应 `/` 入口。全量 `go test ./...`、`go test -race ./...` 和 OpenSpec strict 校验通过，新增选配用例在 race 下连续 3 次通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 配置与确认 | `TestModelPickerUsesConfiguredConnectionsAndDefersAllWrites`、`TestSelectionEmptyStaleAndUnavailableCatalogs` | 同名连接按 ID 区分；空配置、未实现 adapter、目录版本变化和晚到查询明确处理；连接、型号、力度逐级选择，确认才写 |
| 目录与分页 | `TestDirectoryNavigationDotPathAndPaginationKeepRevision`、`TestPickerModelPagesUseTheOriginalVersionAndBoundQueries` | `.`、`..`、根目录、中文空格路径、h/j/k/l 与分页通过；路径按字面传递，保留初始 revision 和目录版本 |
| 回执与 Git | `TestSelectionAcceptanceFencingRetryAndDraft`、`TestGitRefreshCoalescesAndClearsOldOrUnknownState` | 未知接受保留原请求，缺失回执和版本冲突拒绝；切目录、查询失败或非 Git 清除旧分支，旧计时器/回复无效，上游缺失保持未知 |
| 布局与入口 | `TestPickerCellsThemeAndClickCommandEquivalence`、`TestPickerMetadataStaysInOneRowWithoutChangingTargets` | 六种面板 × 三种尺寸 × 三种颜色能力共 54 组通过；输入/底栏保留，点击与命令等价，换行、Tab、控制字节不改变布局或路径参数 |
| 草稿与焦点 | `TestPickerCancellationReturnsToTheSameAnswerComposer`、`TestPickerStillAllowsSkillCommandsBelowTheComposer`、`TestPickerFocusDoesNotInterpretHeldDraftAsCommand` | 回答、文字和图片保留；选择页内 `/`、`$` 可用；草稿不抢导航焦点，粘贴按文本返回输入，方向键不提交选择 |
| 真实终端与 core | `TestRealTerminalSelectsThroughCoreAndRefreshesGit` | 48×16 Linux PTY 经真实 UDS/SQLite 完成模型、力度和非 Git 目录确认；取消保持原设置，仅两条 selection 事件、零任务、零凭据读取；退出恢复 termios 和 alt screen |

```sh
go test -race ./internal/render -run 'TestModelPicker|TestSelection|TestDirectoryNavigation|TestGitRefresh|TestPicker|TestRealTerminalSelects' -count=3 -timeout=90s
```

首轮测试修正了两处夹具假设：图片测试只遍历图片入口；selection revision 按历史 seq 分配，不假定从零逐次加一。边界检查补上单行元数据转义和回答焦点保留。按 tui-design 采用无边框单页下钻，按 trim-cn-tech-doc 合并过时的选择页待办说明，保留历史验证与失败条件。

验证使用临时 Git 仓库、SQLite 和配置元数据，未发送模型请求、读取外部凭据或安装用户服务。V21 的本地交互链路完成，四协议兼容性与整体部署仍按后续任务验收。

## 2026-10-16：持续子 Agent 与调用归因

6.1 已完成，累计 32/68 项。spawn 原子创建子身份、继承上下文和首个 Task，立即返回；submit 沿用原 Agent。全量普通测试、race 和 OpenSpec strict 校验通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 继承与复用 | `TestSpawnAtomicInheritanceCallerAndPersistentReuse`、`TestInheritedContextDoesNotSplitParallelToolGroup` | 完整调用组、图片字节与私有字段保留；未完成尾部排除并记录范围，父上下文不改；重开后同一子 Agent 接收新任务 |
| 原子性与限制 | `TestSpawnRejectsWithoutPartialIdentity`、`TestConcurrentSpawnAcceptanceCreatesOneChild`、`TestSpawnNoneAndDepth` | 来源、容量、深度、选择冲突、损坏上下文及三处提交故障不留部分身份；12 个并发同请求只创建一个子 Agent |
| 归因与跨调用重试 | `TestSpawnCLIAttributionIdempotencyAndReuse`、`TestCollaborationEffectIdentityExcludesRetryCallerOnly` | 真实 UDS/SQLite 核对 exec 来源；同调用自动去重，跨调用显式复用 ID 返回首次回执，改变任务参数报冲突；续任务记录调用来源 |
| shell 环境 | `TestProcessEnvironmentReplacesCaller`、`TestPipeAndPTYReceiveOwnCaller` | pipe 与真实 tmux PTY 使用自身启动来源，覆盖旧环境；无归属进程清除旧绑定 |
| 完整 CLI 链路 | `TestBinaryModelSpawnsThroughPipeAndPTYCLI` | 真实二进制经 exec 调 CLI，重复 spawn 返回同一身份；子模型请求仍挂起时父任务已完成，临时 cd 不改继承 cwd；再次 submit 保留原历史 |
| 提示词 | `TestCollaborationHintsOnlyRegisteredCommands` | 版本 6 的 spawn/submit/inspect 对应实际命令；二进制路径及 profile 正确引用，未宣传 send/wait |

```sh
go test -race ./internal/protocol ./internal/store ./internal/process ./internal/cli -run 'Spawn|Collaboration|InheritedContext|ProcessEnvironment|ReceiveOwnCaller' -count=3 -timeout=120s
```

首轮二进制夹具漏算了请求前的选配通知，且把 PTY 文本投影中的 CR 转义当成原始 JSON；修正夹具后 pipe/PTY 两条链路通过，产品的控制字符转义保持。重试检查另补上“调用来源变化、业务参数不变”的稳定身份测试。

按 Pi 的能力条件组合短提示，按 trim-cn-tech-doc 合并默认值、归因与恢复说明。测试使用临时 XDG、SQLite、runner 和本地 HTTP 模型，子二进制为常规构建，宿主及包内并发由 race 检查。V02 的创建/复用部分完成，长期 mailbox 通信仍待 6.4；6.2–6.7 和用户级部署保持未完成。

## 2026-10-16：上下文选择与只追加视图

6.2 已完成，累计 33/68 项。继承支持 all/none/last:N、预算及文本/文件/原文背景；回执保存实际范围和估算。全量 `go test ./...`、`go test -race ./...`、CLI 帮助和 OpenSpec strict 校验通过，相关用例在 race 下连续 3 次通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 轮次、预算与工具组 | `TestSpawnLastTurnsBudgetAndPendingPairs` | 跨轮并行调用成组保留；未完成尾部排除；显式范围超预算拒绝，仅预算选择最近完整轮次，失败不留身份或回执 |
| 压缩视图与原文背景 | `TestSpawnRawBackgroundAfterCompactionAndImages` | 默认继承摘要及尾部，last:N 只取所选轮次；原文引用可恢复摘要覆盖范围，跨 Agent 图片字节及私有字段保留；缺失端点、孤立结果、未完成组与重复调用拒绝 |
| 只追加与重开 | `TestContextViewAppendOnlyTailInheritanceAndRestart` | 原文不改，历史游标仍解析旧视图；准备期间新增尾部保留，子孙继承不把摘要计为用户轮次；重开保持视图和来源 |
| 发布事务 | `TestContextViewRejectsSplitInvalidRangeAndRollsBack`、`TestContextViewConcurrentPublicationFencesRequests` | 拆分工具组、非法范围及注入写入失败保留旧视图；八个竞争发布仅一个成功，旧请求准备被游标拒绝；仅摘要时报告 0 轮 |
| 实际请求 | `TestModelRequestsPinContextViewAcrossPublication` | 同一 Task 两轮模型夹具请求：流中发布不改原请求，下一轮使用新视图、保留原响应尾部并重申运行设置；两次请求各存对应视图 ID |
| CLI 与迁移 | `TestSpawnCLIAttributionIdempotencyAndReuse`、`TestContextViewJournalMigrationPreservesVersionSevenHistory` | 真实 UDS 验证新参数、stdin 背景和历史引用；schema 7→8 保留原任务与消息，无表重写；默认 CLI 参数维持原重试指纹 |

```sh
go test -race ./internal/store ./internal/runtime ./internal/cli -run 'Test(ContextView|Spawn|ModelRequestsPin|InheritedContext|CollaborationHints)' -count=3 -timeout=90s
```

补测发现 `last:N` 等于保留轮数时误带旧摘要，已修正选择起点；首轮背景夹具复用固定创建 ID 触发去重冲突，改为独立身份后通过。原失败不计为通过。

提示词版本 7 继续参考 Pi 的能力组合短说明，文档按 trim-cn-tech-doc 合并选择规则并保留预算边界。视图发布是存储基础接口，未接自动摘要生成、容量触发或公开 compact 命令，9.1–9.4 仍未完成。模型为本地夹具，未访问真实模型或执行用户级部署。

## 2026-10-16：显式 worktree 与创建续接

6.3 已完成，累计 34/68 项。默认仍取父有效 cwd，显式 worktree 从准备时的 HEAD 创建独立检出，完成后保留目录。全量 `go test ./...`、`go test -race ./...`、spawn 帮助和 OpenSpec strict 校验通过；相关用例在 race 下连续 3 次通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 真实 Git 与结果 | `TestWorktreeRealCreationPreservesSourceAndResults`、`TestWorktreeSpawnRunsAndRetainsDirectoryAfterCompletion` | detached 检出保存提交，父未提交/未跟踪文件不带入；父 HEAD、refs、索引与文件不变；子任务完成后产物留在独立目录，重试返回原回执 |
| 路径与归属 | `TestWorktreeRejectsInvalidOrReplacedDestinations` | 非 Git、无 HEAD、已有目录/链接、Git 管理目录、缺失父目录拒绝；目标出现、父路径被替换、所有者标记或 `.git` 指向另一检出时不采用 |
| 持久意图与事务 | `TestWorktreeIntentReceiptRetryAndGlobalOperationIdentity`、`TestWorktreePreparationPreflightAndTransactionalFailures` | 首次调用归因保持，预留 ID 不能用于另一操作；容量、上下文、深度及事务故障不留半个子 Agent；最终接受失败保留 ready，重试可续原目录 |
| 中断窗口 | `TestWorktreeSpawnRecoversOnlyOwnedCompleteCheckout`、`TestWorktreeLockSurvivesOwnerClose` | 重开后续 prepared，核实原干净检出；无结果记录且目录缺失/脏时保存 unknown，不重复创建；已保存 ready 允许后续编辑；父描述符关闭后存活子进程仍持锁 |
| 真实 core 崩溃 | `TestBinaryWorktreeRecoversAfterCoreSIGKILL` | Git 检出中途 SIGKILL 测试 core；新 core 在原 Git 完成前返回 busy，完成后原 ID 恢复同一路径和提交；只有一次检出、一个子 Agent，后续回执不变 |
| 迁移、CLI 与提示词 | `TestWorktreeSchemaEightMigrationPreservesContextView`、`TestSpawnCLIAttributionIdempotencyAndReuse`、`TestCollaborationHintsOnlyRegisteredCommands` | schema 8→9 保留原视图；无效参数拒绝，默认共享的重试指纹不变；版本 8 说明对应已注册参数与保留、恢复规则 |

`TestBinaryModelSpawnsThroughPipeAndPTYCLI` 同时作为回归，继续验证 shell 临时 cd 不改变子 Agent 默认 cwd。

```sh
go test -race ./internal/workspace ./internal/store ./internal/runtime ./internal/cli \
  -run 'Worktree|TestCollaborationHints|TestSpawnCLI' -count=3 -timeout=120s
```

本节覆盖 V04 的本地实现。实际测试 Git 为 2.55.0；Git、SQLite、UDS 和 core 为真实实现，子任务完成使用模型夹具，崩溃检出用例不访问模型。子二进制为常规构建，宿主与包内并发另由 race 检查。未使用真实凭据或执行用户级部署，断电持久性与树级停止不在本节验收内。

提示词按 Pi 的能力条件组合，文档按 trim-cn-tech-doc 合并默认值与恢复说明，保留目录预留、计时器、Git 过滤器和不确定结果的边界。

## 2026-10-16：持久 mailbox 与 queue/steer

6.4、6.5 已完成，累计 36/68 项。schema 10 保存消息、来源、接收 Task 和投递游标；接受与投递分别提交，查询不消费。全量 `go test ./...`、`go test -race ./...`、CLI 帮助和 OpenSpec strict 校验通过；新增协作、mailbox 与显示用例在 race 下连续 3 次通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 接受、来源与去重 | `TestMailboxAcceptanceAttributionRetryAndReadOnlyQueries`、`TestSpawnCLIAttributionIdempotencyAndReuse` | 八路并发同 ID 只接受一次；人类/模型来源区分，跨调用重试保留首次归因；收件/发件查询只读，重开后原消息与处理结果保持 |
| 事务与迁移 | `TestMailboxAtomicAcceptanceDeliveryAndSchemaNineMigration`、`TestMailboxSteerBatchRollbackAndSelfSource` | 接受回执、正文投递、游标和事件四处故障完整回滚；一批 steer 第二条失败也不留下第一条投递；自发自收的两侧 seq 正确；9→10 保留原快照 |
| 调度、暂停与顺序 | `TestMailboxPauseResumeOrderPriorityAndRequestFence`、`TestMailboxPausedAndIdleSchedulingBothModes` | 忙碌、空闲、暂停、unknown 均有验证；resume 按已有消息接收顺序，显式 submit 优先；暂停中的消息不唤醒，重复 resume 不更换预留 Task，迟到 steer 拒绝过期请求准备 |
| 完整工具组 | `TestMailboxQueueSteerWaitForWholeToolGroupAndKeepOneLoop` | 两个受控工具全部完成后 steer 才进入下一请求，原工具未取消；queue 在原 Task 完成后启动新 Task；三次模型请求、两个工具派发，无第二条并行循环 |
| 限额与分页 | `TestMailboxValidationPaginationAndCapacity` | UTF-8、256 KiB 正文、128 条待投递上限、活动 Agent 容量、状态与游标校验通过；列表只含元数据，旧消息和历史不被删 |
| 显示与重放 | `TestMailboxProjectionAttributionLifecycleAndWidth`、`TestMailboxProjectionRejectsMalformedEventsAndBoundsRetention` | 80×24、60×24、48×16 保留发送者及状态，正文只显示一次；控制符转义、宽字符、裁切预算、错误事件和未知处理状态通过；无新面板或按钮 |
| 真实 core 崩溃 | `TestBinaryMailboxSurvivesAcceptanceAndDeliveryCrashes` | 暂停中接受后 SIGKILL，重启仍暂停；resume 投递后再次 SIGKILL，重启继续原 Task；同 ID 重试返回原消息，一个 Task、一次上下文投递；CLI 读取的实际历史可由 TUI 同源重放 |

`processed` 只表示关联 Task 已完成，不是外部业务验收。失败、取消和待核实单独保留在 `task_state`，已进入上下文的消息不会因处理失败自动重投。

```sh
go test -race ./internal/render ./internal/store ./internal/runtime ./internal/cli \
  -run 'Mailbox|SpawnCLI|CollaborationHints' -count=3 -timeout=120s
```

首轮夹具修正了旧 schema 模拟时遗漏移除 mailbox 表、工具结果字段与 role 的假设，以及将 steer 显示误断言为 queue；原失败不计为通过。

提示词版本 9 继续按 Pi 的已注册能力组合，区分接受、投递和处理完成；按 trim-cn-tech-doc 合并规则并保留限额与恢复条件。tui-design 用于复用输出区和窄屏检查，零新增边框/按钮，邮箱状态只用一处文字标记。Git、SQLite、UDS 和二进制是真实本地组件；模型为本地夹具，未使用外部凭据或执行用户级部署。

## 2026-10-16：子任务结果回流与 wait

6.6 已完成，累计 37/68 项。schema 11 保存结果来源、wait 观察关联和消息投递关系；final 与回流消息同事务提交，完整正文进入工具上下文后才去重。全量 `go test ./...`、`go test -race ./...`、wait/result 帮助和 OpenSpec strict 校验通过；新增用例在 race 下连续 3 次通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 完成与投递事务 | `TestTaskResultAtomicReturnWaitReceiptAndContextConsumption` | 注入回流写入、投递游标失败均完整回滚；重开保留观察关联；仅有引用、重复投递拒绝，人类查询不改变历史 |
| 自动回流与暂停 | `TestTaskResultPauseQueueAndNoAutomaticAcknowledgement` | 子 final 保留来源与原记录，暂停不唤醒；纯通知处理不自动回执，正常业务 Task 仍回报结果 |
| 父等待与结果身份 | `TestTaskWaitLoopDoesNotWaitForParentMailboxOrRepeatDelivery`、`TestTaskWaitWriteStdinUsesOriginalProcessAndRecipient` | 子任务完成即可返回；原 process ID 关联 exec/续读，其他 Agent 不误消费；一个父 Task、一次投递、无重复唤醒 |
| 显式恢复与并发结果 | `TestTaskResultWaitKeepsExplicitResumeAndConsumesMultipleResultsAtomically`、`TestTaskWaitHonorsResumeBeforeOldToolSettles` | 多个结果可进入同一工具上下文；停止后立即 resume 保留已接受的新 Task，旧 Task 取消，正文不再投递，也不添加空 user 消息 |
| 超时、分页与容量 | `TestTaskWaitTimeoutCancellationFailureAndLargeResult`、`TestTaskResultPagingValidationCapacityAndSchemaTenMigration` | 观察超时/取消不停止子任务；1 MiB 结果超工具预算时继续 queue，UTF-8 页可还原原文；send 达限不丢 final，10→11 保留旧消息与历史 |
| CLI 与显示 | `TestSpawnCLIAttributionIdempotencyAndReuse`、`TestTaskResultProjectionShowsOneBody` | wait/result 经 UDS 查询，非法参数拒绝；80、60、48 列只显示一份结果，保留发送者、状态及原始工具记录 |
| 真实 core 崩溃 | `TestBinaryTaskWaitRecoversCLIOutputBeforeContext` | CLI 已输出完成引用、工具尚未入上下文时 SIGKILL；重启沿用原 Task/进程，正文投递一次，命令效果一次，实际历史可由 TUI 重放 |

```sh
go test -race ./internal/runtime ./internal/store ./internal/render ./internal/cli \
  -run 'TaskWait|TaskResult' -count=3 -timeout=120s
```

首次真实链路复现同一结果在工具与 mailbox 重复显示，已增加明确的消息引用供显示去重，原始记录与模型上下文保留。复查 stop/resume 后，修正了去重取消用户续接 Task 的问题。测试夹具另修正旧 schema 遗留 wait 表、显示事件 Task ID 不匹配；原失败不计通过。

提示词版本 10 参考 Pi 的按能力组合短说明，文档按 trim-cn-tech-doc 合并过时待办并保留预算、游标、暂停与恢复条件。tui-design 用于沿用输出区和窄屏验证，零新增边框或按钮。二进制、SQLite、UDS、pipe/tmux 为真实本地组件；模型为本地 HTTP 夹具，子二进制为常规构建，宿主与包内并发由 race 检查。V18 本地链路完成，未进行真实供应商或用户级部署验收。

## 2026-10-16：树级停止的持久准备

6.7 部分实现，完成数仍为 37/68。schema 12 用追加事件保存停止范围与成员冻结状态；旧版本程序拒绝打开该版本，避免忽略停止约束。接口暂留存储层，尚未注册树级停止 CLI。全量 `go test ./...`、`go test -race ./...` 和 OpenSpec strict 校验通过；相关存储测试在 race 下连续 3 次通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 范围、重试与重开 | `TestTreeStopCapturesScopeAndPersistsAdmissionFence` | 根、子、孙同事务暂停；队列、未读消息和原上下文保留，范围外 Agent 不变；同 ID 重试不追加，重开后仍阻止 resume、submit、send、spawn 和选配 |
| worktree 范围 | `TestTreeStopCapturesAllPendingWorktreeStates` | prepared/creating/unknown/ready 均纳入；已接受检出由子 Agent 覆盖；阻止新准备和派发，允许旧结果落定，禁止发布新子 Agent |
| 事务失败与重叠 | `TestTreeStopOverlapAndFailureRollBackWholeScope` | 成员冻结、范围事件、回执写入失败全部回滚；重叠停止返回原操作身份，只读成员拒绝整项操作 |
| 并发接受 | `TestTreeStopSpawnRaceIncludesOrRejectsChild`、`TestTreeStopWorktreePreparationAndAcceptanceRaces`、`TestTreeStopSubmitAndResumeRacesCannotReleaseFence` | 并发派生/准备/接受要么纳入范围，要么拒绝；提交与恢复不能在停止提交后解除暂停 |
| 未知、容量与迁移 | `TestTreeStopRequiresExplicitMatchingScopeAndKeepsUnknown`、`TestTreeStopBoundRejectsWithoutPartialPause`、`TestTreeStopSchemaElevenMigrationPreservesFacts` | 树范围必须显式且匹配目标；未知效果仍待核实；超过 10000 个 Agent 整项拒绝；11→12 保留旧快照 |
| 完成记录投影 | `TestTreeStopGateReleasesOnlyOnOriginalRootCompletion` | 仅原根操作的完成事件解除全树冻结，成员记录或别的操作无效；解除后仍人工暂停，旧 stop 重试不取消新工作。完成事件由夹具写入，不是进程退出验收 |

Task 结果可继续落盘，但 Task 全部终态也不会把树操作自动改成 stopped。runner 停止、Git 身份与锁核实、停止流程的崩溃续接和公开命令仍待接通；本节不代表整树终止或部署验收通过。

复现：

```sh
go test -race ./internal/store -run 'TreeStop|StopResume|StopAnd|MailboxPause' -count=3
```

说明按 trim-cn-tech-doc 区分“接受停止”和“已核实退出”，保留范围、上限和恢复条件。新增存储测试只使用隔离 SQLite 与夹具，没有发出进程终止信号或访问外部模型。

## 2026-10-16：runner 的持久范围停止

6.7 继续实施，完成数仍为 37/68。runner 新增冻结、推进、查询和释放接口，进程启动与 stdin/resize 共用冻结检查；尚未接入 core 的树级停止流程或 CLI。全量 `go test ./...`、`go test -race ./...` 和 OpenSpec strict 校验通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 冻结、终止与迟到请求 | `TestScopeStopPipeFreezeTerminateAndLateDispatch` | 原进程可查、旧回执可重试；新启动/输入拒绝，范围外进程继续；核实终止后输出保留，单独释放后迟到 ID 仍不能执行，旧 stop 不影响新工作 |
| 并发与输入顺序 | `TestScopeStopConcurrentStartIsCapturedOrRejected`、`TestScopeStopFreezeWaitsForAdmittedInput` | 启动要么纳入范围，要么拒绝；冻结等已进入的 stdin 落定，不允许后续写入越过停止 |
| PTY 与真实 runner 崩溃 | `TestScopeStopPTYAndRunnerReopen`、`TestScopeStopSurvivesRealRunnerSIGKILL` | 真实 tmux job-control 组终止，原 pane/输出保留；独立 runner 冻结后被 SIGKILL，新进程恢复原范围、终止 pipe/PTY、继续拒绝迟到命令 |
| 身份与后代 | `TestScopeStopUnknownIdentityNeverSignalsOrReleases`、`TestScopeStopSavedWitnessSurvivesLeaderExit` | 身份不符时不发信号、不释放；保存的成员身份可在 leader 已退出后证明原组，重开后仍可终止后代 |
| 扫描后派生 | `TestScopeStopLateForkUsesOriginalGroupHandle`、`TestScopeStopGroupFallbackUsesSameHandleOnly` | 确定性地在保存扫描结果后派生子进程，原 pidfd 组终止覆盖它；旧内核回退夹具只使用同一描述符，不改用裸 PID |
| 损坏、失败与容量 | `TestScopeStopInvalidMetadataHoldsAdmission`、`TestScopeStopFailedSaveNeverAllowsNewEffects`、`TestScopeStopUnknownOwnershipAndCapacityAreAtomic` | 损坏/未来版本/不完整范围/虚假完成/链接/目录拒绝；保存不明时保留冻结；未知归属、范围冲突和超限不留下部分停止 |
| 身份保留与额度 | `TestScopeStopKeepsProcessIdentityWhenMetadataDisappears`、`TestScopeStopProvedExitReleasesCapacityWithoutInventingResult` | 已停止进程元数据缺失也不重用 ID；核实无活动进程后释放并发额度，崩溃 pipe 的退出码仍未知 |
| IPC 与重试 | `TestScopeStopRemoteContract` | 真实 Unix socket 查询同一记录；参数顺序不改变范围，冲突 ID/错误目标拒绝，释放重试不重启或重停工作 |

首轮全量 race 复现扫描与 shell 派生竞争：逐个终止已扫描 PID 会留下迟到子进程。尝试暂停后扩展范围时，又验证了 tmux 自动恢复暂停 pane 的行为。最终改用 pidfd 进程组信号；修正后的停止用例在 race 下连续十轮通过，补充确定性派生和旧内核回退测试后再通过三轮。旧失败不计通过，机制和来源见 [树级停止底层接口](./processes.md#树级停止底层接口)。

```sh
go test -race ./internal/process -run ScopeStop -count=3 -timeout=90s
```

pipe、tmux、UDS、文件和 runner 崩溃为真实本地测试；独立 runner 使用本包测试二进制，继承 race 构建。当前内核为 6.18.54.1-microsoft-standard-WSL2；旧内核回退仅用调用夹具验证，未实际运行旧内核。没有访问模型、写入生产配置或进行用户级部署。

说明按 trim-cn-tech-doc 合并状态解释，保留范围、限额、失败和旧内核边界。存储层的树计划与 runner 停止尚未串联，Git 终止和整个流程的恢复仍未验收，不能据此宣称整棵 Agent 树已可停止。

## 2026-10-16：worktree 的可恢复停止步骤

6.7 继续实施，完成数仍为 37/68。Git 创建使用独立进程组，PID 身份和停止证据追加到原历史；确认组已结束且继承锁释放后，才保存 cancelled。全量 `go test ./...`、`go test -race ./...` 和 OpenSpec strict 校验通过。内部步骤已实现，尚未注册树级停止 CLI 或接入总调度。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 活动检出与文件保留 | `TestWorktreeStopTerminatesLiveCheckoutAndRetainsFiles` | 终止真实 Git/过滤器，保留未跟踪修改与原目录；同 ID 不重跑、不发布子 Agent，树操作仍为 stopping |
| 派发窗口与锁 | `TestWorktreeStopWaitsForLateIdentityAndInheritedLock` | Git 已启动、身份尚未保存时返回 busy；迟到身份可保存，随后结束原组并落定取消 |
| 真实 core 崩溃 | `TestWorktreeStopRecoversAfterRealCoreSIGKILL` | 测试 core 在检出中途被 SIGKILL；新 core 从 SQLite 读取原身份，终止存活的 Git/过滤器并保留目录，检出仅一次 |
| 先记录后终止 | `TestWorktreeStopDoesNotSignalBeforeEvidenceCommit`、`TestTrackedWorktreeIdentityFailureDoesNotAbandonLiveGroup` | 停止证据写入失败不发信号；启动身份保存失败尽力终止本次创建，返回脱敏错误并释放已结束组的锁 |
| 持久证据与原子取消 | `TestWorktreeStopIdentityEvidenceAndCancellation`、`TestWorktreeStopRejectsScopeChangesAndRollsBack`、`TestWorktreeProcessWriteFailureLeavesNoPartialIdentity` | 原进程身份不可替换，重复观察不增历史；重开保留成员证据，越界操作及未核实取消拒绝，事件故障完整回滚；整树解除冻结后旧创建仍不能复活 |
| 缺失与错误身份 | `TestWorktreeStopMissingIdentityAndMalformedEvidence`、`TestExternalGroupSharesVerifiedKernelStop` | 缺少身份由运行时锁检查约束；损坏记录不当作无进程，PID 生命周期不符不发信号；复用原 pidfd 停止逻辑 |

新增用例在 race 下连续十轮通过；补充停止证据提交失败测试后再通过三轮，缺失进程身份的停止记录补测也通过三轮。后者确保解码不沿用原对象字段，不把不完整记录当作已核实退出。

```sh
go test -race ./internal/process ./internal/workspace ./internal/store ./internal/runtime \
  -run 'WorktreeStop|WorktreeProcessWrite|TrackedWorktree|ExternalGroup' -count=3 -timeout=90s
```

崩溃用例使用独立的本包测试进程，继承 race 构建；Git、过滤器、文件锁和 SQLite 为真实本地组件。启动至身份保存的窗口使用确定性暂停夹具。没有访问模型、修改参考仓库或写入生产配置。说明按 trim-cn-tech-doc 合并状态解释，保留目录、未知身份及原进程组的边界。

## 2026-10-16：整树停止、Task 核实与 CLI

6.7 已完成，累计 38/68 项。core 从持久停止计划推进 runner、Git 与 Task 核实，CLI 提供显式 `agent stop --tree` 和实时操作查询。全量 `go test ./...`、`go test -race ./...`、stop 帮助及 OpenSpec strict 校验通过。

| 验证 | 入口 | 结果 |
| --- | --- | --- |
| 树范围与后续工作 | `TestTreeStopOwnsWholeTreeButPreservesUnrelatedWorkAndQueuedInput` | 父、子、孙及所属 pipe/PTY 结束，范围外 Task/进程继续；队列与消息保留，resume 只恢复目标，旧 stop 不影响新工作 |
| 不可取消与未知效果 | `TestTreeStopWaitsForUncancellableToolSettlement`、`TestTreeStopDoesNotClearUnknownPatch` | 原工具未返回时保持 cancelling；未知 patch 保持冻结，runner 结束不冒充工具效果核实 |
| 迟到命令与无模型恢复 | `TestTreeStopSettlesNeverStartedExecWithoutReplaying`、`TestRecoverStoppedUndeliveredExecNeverDispatchesOrInspects` | 接受后重开 core，无模型也可取消原 Task；原命令未执行，释放后迟到 RPC 仍拒绝，只保存一次核实结果 |
| 真实 core/runner 崩溃 | `TestTreeStopSurvivesCoreAndRunnerSIGKILL` | runner 丢失 pipe 描述符后重新接入，core 冻结范围后被强杀，runner 再次强杀；恢复完成原范围，命令效果一次、原输出可读、pipe 退出码未知 |
| 完成事务与释放窗口 | `TestTreeStopRecoversReleasedRunnerBeforeCompletionCommit`、`TestTreeStopCompletionRequiresReleasedMatchingReceiptAndSettledTasks` | runner 已释放而数据库提交失败时，Agent 仍冻结；重开用原回执补完成，错误范围、未落定任务、未释放 runner 均不能宣告完成 |
| Git 与服务不可用 | `TestTreeStopDriverCancelsWorktreeWithoutModel`、`TestTreeStopRetriesUnavailableRunnerWithoutUnpausing`、`TestTreeStopRejectsUnavailableRunnerBeforePausing` | 无模型时终止原 Git/过滤器并保留检出；缺停止能力先拒绝，暂时离线保存失败阶段并重试，已接受重试不依赖当前服务可用性 |
| 范围快照与限额 | `TestTreeRunnerScopeIsImmutableAndCapturesDispatchedIdentities`、`TestTreeRunnerScopeDoesNotSilentlyTruncateDuplicateEvents`、`TestPendingTreeStopsPaginationAndProgressDedup` | 历史/旧版派发纳入，准备态排除；重开和重试范围不变，重复事件不挤掉后续 ID，超 10000 项不保存部分范围；分页和相同进度去重通过 |
| 工具核实与身份 | `TestRecoverStoppedRetainsUnknownExitAndAvailableOutput`、`TestRecoverStoppedRejectsIncompleteProofAndInput` | 已保存输出与未知退出码同时保留；错参数、PID、任务、未完成证明及输入调用不能借停止证据清除未知状态 |
| CLI 与提示词 | `TestCLIExplicitTreeStopAndLiveInspection`、`TestCollaborationHintsOnlyRegisteredCommands` | 真实 UDS 验证默认单 Agent、显式树范围、重试、完成查询与 JSON/错误通道；resume 拒绝 tree 参数，说明核对注册命令与默认值 |

```sh
go test ./internal/runtime -run TestTreeStopSurvivesCoreAndRunnerSIGKILL -count=1 -v
go test ./internal/cli -run 'TestCLIExplicitTree|TestCollaborationHints' -count=1
go test -race ./internal/runtime ./internal/tool ./internal/store \
  -run 'TestTreeStop|TestTreeRunner|TestPendingTree|TestRecoverStopped' -count=3
```

第一批树停止与核实用例在 race 下连续三轮通过，补充离线重试、不可取消工具及 CLI 后再通过全量 race。首轮测试修正父 selection revision 和未知观察夹具；范围扫描修正为先去重再检查上限，避免重复历史事件导致漏项。旧失败不计通过。

提示词版本 11 继续参考 Pi `7bc154ad082e2f0029c19117a16d22a504439476`，仅给已注册 exec 能力加入协作命令；旧 Task 的已存 prompt 保留。tui-design 用于显式 CLI 参数与输出分流，未增加界面按钮。trim-cn-tech-doc 合并过时待办说明，保留范围、限额、失败条件和历史验证记录。

崩溃测试的独立 core/runner 使用本包测试二进制，继承 race 构建；Git、SQLite、UDS、pipe/tmux 为真实组件，模型使用确定性夹具。未使用外部凭据、修改参考仓库或部署用户服务；主动脱离原进程组/session 的守护程序仍不在停止范围内。

## 进行中

继续第 7 组的资源热加载、MCP 搜索调用及 skill 请求级更新。会话复制仍依赖第 9 组的视图和恢复能力。Day1 整体和本机部署尚未完成；推送仍需配置用户指定的 remote。

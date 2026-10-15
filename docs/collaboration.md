# Agent 创建与复用

已实现 spawn、worktree、后续任务提交、调用归因和持久去重。Agent 由 core 调度，CLI 返回身份后退出，子 Agent 不另开 TUI。

## 用法

人类从已有会话派出任务：

```sh
zende --profile dev --request-id review-1 agent spawn --parent PARENT --task-file TASK.md
zende --profile dev agent inspect CHILD
zende --profile dev --request-id review-2 agent submit CHILD --file FOLLOWUP.md
zende --profile dev agent history CHILD --after 0
```

模型在 exec_command 中可省略 `--parent`。spawn 的 `--task-file`（别名 `--file`）支持文件或 `-` 读 stdin，也可直接传文字；上限 2 MiB UTF-8。回执含 Agent、首个 Task、操作身份和继承范围。`accepted` 表示任务已保存，结果从 inspect/history 查询。

submit 给同一 Agent 增加 Task，保留历史；忙碌时排队，人工暂停时显式 submit 可唤醒。它还不是待实现的 send/mailbox 投递接口。

## 继承

默认继承父 Agent 当前视图：已有摘要和保留消息，不自动展开被压缩的原文。正在执行的父调用及其未配对尾部留在父会话，完整调用组保持原顺序。

| 参数 | 选择方式 |
| --- | --- |
| `--context all` | 当前视图的完整消息前缀，默认值 |
| `--context none` | 不继承父消息；仍可补充背景 |
| `--context last:N` | 从最近 N 个 user 轮次的起点取消息；摘要不计轮次，工具组跨轮时向前保留到安全边界，报告实际轮数。只有摘要时保留摘要并报告 0 轮 |
| 仅 `--context-budget TOKENS` | 在估算预算内选择最近的连续完整轮次；最新一轮放不下则报错 |
| 显式模式加预算 | 必须完整容纳指定范围，超出返回 `context_budget`，不擅自缩减 |

背景可用 `--background TEXT` 或 `--background-file FILE`（二选一，最多 2 MiB UTF-8）。文件可为 `-`，但任务和背景不能同时读 stdin。`--background-history AGENT:FIRST:LAST` 按原事件序号包含两端的原始 `context.message`，可重复指定，最多 16 段；两端必须存在，工具调用与结果必须完整。引用可来自其他 Agent，也可取回已被摘要覆盖的原文。重叠引用造成调用 ID 重复时拒绝。

预算只计继承消息与补充背景，不含子身份、新任务、system、工具定义及输出预留。当前估算按每条消息 JSON 字节数除 4 向上取整，再加 8；每次图片引用另加 1200。方法随回执记录，包含私有字段，但不是供应商用量或模型容量保证；完整请求容量控制仍归 9.2。

回执 `spawn` 保存父游标、视图 ID、是否包含当前摘要、原文首末序号、消息/轮数、两类排除数、背景范围及预算估算。当前摘要用视图 ID 标识，不冒充原消息；仅含当前摘要或无继承时首末序号省略。子事件 `context.inherited` 保存同一来源，父事件 `agent.spawned` 指向新身份；两边后续独立追加。

模型私有字段保持原值；图片复制已保存字节，记录原引用到子引用的映射。继承内容与背景不成为活跃请求或问题；末尾追加子身份说明，再接受独立新任务。父历史、待执行任务和运行进程保持原位。

模型请求与 spawn 共用视图读取入口。已实现的存储接口只追加摘要、原文范围和旧视图引用，保留准备期间新增的尾部；发布与事件游标同事务提交，请求记录实际视图 ID。视图切换后重新追加当前运行设置，已发请求不变。自动生成摘要、容量触发与公开 compact 命令仍待第 9 组。

cwd 默认取创建时父 Agent 的已接受设置，`--cwd PATH` 显式选择其他现有目录，相对路径仍按父 cwd 解析。shell 临时 cd、父 Agent 后续切换目录均不移动子 Agent。模型和力度继承父的显式选择；未选配则使用 profile 默认值。创建前检查并发容量和递归深度，根 Agent 深度为 0；失败不留下新身份。

## Worktree

默认 `--workspace shared`。需要独立检出时明确给出新目录：

```sh
zende --profile dev --request-id isolated-1 agent spawn --parent PARENT \
  --workspace worktree --worktree-path ../review-copy --task-file TASK.md
zende --profile dev operation inspect isolated-1
```

源目录默认取父有效 cwd，`--cwd` 可覆盖。目标相对源目录解析，父目录须已存在，目标须不存在；已有空目录或链接也拒绝。非 Git 仓库、尚无 HEAD 等条件不满足时直接报错。

准备记录固定源目录、Git 公共目录、HEAD 提交、目标路径和所有者标记。使用 `git worktree add --detach --lock` 检出该提交，不复制未提交/未跟踪文件，不创建分支。子 Agent 接受后返回独立 cwd 与 `workspace` 记录，完成后保留目录和结果，合并、解锁与清理由用户决定。参数依据 [Git worktree 文档](https://git-scm.com/docs/git-worktree)。

创建前先检查上下文、调用来源和容量；接受子 Agent 前再次检查，并继承此时的上下文与模型设置。检出期间父目录或 HEAD 变化，不改变已保存的源和目标。若最终接受失败，保留已准备的 worktree，不留下半个子 Agent。

`operation inspect` 在接受前返回 `prepared / creating / ready / unknown`；接受后返回原 Agent/Task 回执。`creating` 表示已记录派发意图，不保证进程仍活着。重试必须复用原 ID 和参数：

- Git 继承操作文件锁；core 退出后原 Git 仍运行时，重试返回 `worktree_busy`。
- 原操作没有成功记录时，只核实原目录的仓库、所有者标记、保存提交、detached 状态和干净检出；无法确认则保留 `unknown`，不重建。
- `ready` 已确认创建成功，允许后续修改；接受前仍检查目录归属。已接受的重试直接返回原回执。

core 存活时，创建等待上限为两分钟；CLI 等待上限仍为十秒，客户端超时不取消后台准备。core 崩溃后原 Git 的超时计时不再生效，以实际锁释放为准。每个 profile 保留目标预留与创建历史，不能换 ID 重用该目标。

Git 自身的失败清理由 Git 决定，harness 不执行 reset、prune 或目录删除。创建禁用 hooks、fsmonitor 和自动 GC；检出仍遵循可信仓库配置的过滤器，不构成沙箱。跨 core 崩溃续接覆盖进程退出，不作为断电后的文件持久性保证。

## 归因与重试

runner 在 pipe 和 PTY 的 shell 环境中写入 `ZENDE_CALLER`：启动它的 Agent、Task 与宿主执行操作 ID。CLI 自动携带；core 从原 `tool.dispatching` 核对模型请求和工具调用，未知来源或 parent 不符时拒绝。该信息用于归因，不是权限或安全隔离。长命 shell 续写仍归于最初的执行来源，环境不随每次 stdin 改写。

同一次命令内，spawn/submit 未指定请求 ID 时，由调用来源与规范化参数生成稳定 ID。重试返回原回执；有意创建内容相同的多个任务需分别指定 `--request-id`。跨工具调用重试也应显式复用原 ID：新的归因信息不改变原接受身份，历史仍保留首次来源；任务参数变化则报冲突。人类命令默认生成新 ID。

子身份、选择、继承内容、首个任务、父来源事件和接受回执同事务保存。断线可用 `operation inspect ID` 查询；已接受重试不重新检查父当前设置、路径或容量。

## 后续范围

send、mailbox、queue/steer、自动结果回流及 wait 去重归 6.4–6.6。树级停止（含尚未接受的工作目录准备）和全部协作提示词验收归 6.7。当前命令帮助与提示词只列已注册能力。

本地链路复现：

```sh
go test ./internal/cli -run TestBinaryModelSpawnsThroughPipeAndPTYCLI -count=1 -v
go test ./internal/cli -run TestBinaryWorktreeRecoversAfterCoreSIGKILL -count=1 -v
```

测试真实二进制、SQLite、UDS、pipe 和 tmux，模型使用本地 HTTP 夹具。子请求保持运行时父任务可完成，重复 spawn 只产生一个子 Agent，再次 submit 沿用原历史；不代表真实供应商或用户级部署验收。

worktree 用例在真实 Git 检出中途终止测试 core，重启后先验证忙碌拒绝，再释放检出并用原 ID 续接；只创建一次目录和子 Agent，不调用外部模型。

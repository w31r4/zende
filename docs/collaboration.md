# Agent 创建与复用

已实现 spawn、后续任务提交、调用归因和持久去重。Agent 由 core 调度，CLI 返回身份后退出，子 Agent 不另开 TUI。

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

默认 `--context all` 复制父 Agent 当前完整消息前缀，`--context none` 只追加子身份说明和新任务。正在执行的父调用及其未配对尾部留在父会话，不移交或重新派发；完整调用组保留原顺序。

回执的 `spawn` 给出父游标、首末消息序号、实际消息数及排除的未完成尾部消息数。子事件 `context.inherited` 保存同一来源，父事件 `agent.spawned` 指向新身份；两边后续独立追加。继承消息不成为子 Agent 的活跃请求或问题。

模型私有字段保持原值；图片复制已保存的字节，并记录旧引用到子引用的映射。父历史、待执行任务、问题和运行进程不复制为子任务。模型请求与 spawn 共用上下文读取入口；压缩视图接入后也须通过该入口选择。

cwd 默认取创建时父 Agent 的已接受设置，`--cwd PATH` 显式选择其他现有目录，相对路径仍按父 cwd 解析。shell 临时 cd、父 Agent 后续切换目录均不移动子 Agent。模型和力度继承父的显式选择；未选配则使用 profile 默认值。创建前检查并发容量和递归深度，根 Agent 深度为 0；失败不留下新身份。

## 归因与重试

runner 在 pipe 和 PTY 的 shell 环境中写入 `ZENDE_CALLER`：启动它的 Agent、Task 与宿主执行操作 ID。CLI 自动携带；core 从原 `tool.dispatching` 核对模型请求和工具调用，未知来源或 parent 不符时拒绝。该信息用于归因，不是权限或安全隔离。长命 shell 续写仍归于最初的执行来源，环境不随每次 stdin 改写。

同一次命令内，spawn/submit 未指定请求 ID 时，由调用来源与规范化参数生成稳定 ID。重试返回原回执；有意创建内容相同的多个任务需分别指定 `--request-id`。跨工具调用重试也应显式复用原 ID：新的归因信息不改变原接受身份，历史仍保留首次来源；任务参数变化则报冲突。人类命令默认生成新 ID。

子身份、选择、继承内容、首个任务、父来源事件和接受回执同事务保存。断线可用 `operation inspect ID` 查询；已接受重试不重新检查父当前设置、路径或容量。

## 后续范围

last:N、预算、补充背景和压缩视图归 6.2；worktree 归 6.3；send、mailbox、queue/steer、自动结果回流及 wait 去重归 6.4–6.6。树级停止和全部协作提示词验收归 6.7。当前命令帮助与提示词只列已注册能力。

本地链路复现：

```sh
go test ./internal/cli -run TestBinaryModelSpawnsThroughPipeAndPTYCLI -count=1 -v
```

测试真实二进制、SQLite、UDS、pipe 和 tmux，模型使用本地 HTTP 夹具。子请求保持运行时父任务可完成，重复 spawn 只产生一个子 Agent，再次 submit 沿用原历史；不代表真实供应商或用户级部署验收。

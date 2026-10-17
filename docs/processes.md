# 进程 runner

独立 runner 支持 pipe 与 tmux PTY、身份查询、输出游标、输入、尺寸调整、进程组信号和有界等待。

2026-10-16 起按项目约定不新增、不运行测试。文末用例是历史记录；本次配置刷新及进程快照改动未测试或构建。

```sh
./bin/zende --profile dev runner start
./bin/zende --profile dev runner status
./bin/zende --profile dev --request-id process-demo process exec 'sleep 1; printf done'
./bin/zende --profile dev process inspect process-demo
./bin/zende --profile dev process wait process-demo --wait-ms 3000
./bin/zende --profile dev process output process-demo --stream stdout --offset 0
```

PTY 示例：

```sh
./bin/zende --profile dev --request-id terminal-demo process exec --pty --cols 90 --rows 30 'test -t 0 && stty size'
./bin/zende --profile dev process wait terminal-demo --wait-ms 3000
./bin/zende --profile dev process output terminal-demo --stream pty
```

PTY 依赖 PATH 中的 tmux，最低已测版本为 **3.3a**。缺失时返回安装提示，不退回 pipe。每个 profile 使用独立 socket 和空 tmux 配置，不接管用户原有 server。初始尺寸默认 80×24，每轴允许 2–1000；pipe 不接受尺寸参数。

runner 独立于 core。core 或 CLI 退出不终止命令；新 core 可通过 runner 查询原进程。`runner serve` 是前台监督入口，日志在 profile 的 `runner.log`，控制 socket 与 core 分开。

`process exec` 返回真实启动状态，不等待命令完成。`process wait` 最长等待 30 秒，超时只结束本次等待。空输出与进程终态分别表示。重复相同 `--request-id` 与参数返回原进程；同 ID 改参数报冲突，不再执行。

## 配置刷新

`config refresh` 发布 Core 配置；runner 通过只读 `config.runner` 获取 shell、进程容量、输出限额和发布身份。后台每秒触发一次同步，Core 查询限时 2 秒；接受新进程前再同步。设置持久保存后才切换，降额不终止已有进程。`runner status` 返回实际设置、`publication`、`configuration_state` 和最近错误，Core 接受刷新不代表离线 runner 已同步。

首次启动且 Core 不可用时，runner 可从本地配置初始化，状态为 `local`；连接 Core 后以其发布为准。`current` 表示上次同步成功，`stale` 表示保留旧版但同步失败，`unavailable` 表示还没有可用设置，`invalid` 表示保存记录损坏或不兼容。连接过 Core 的 runner 同步失败时拒绝新进程；已有进程的观察、控制和原 ID 重试继续可用。首次配置失效也保留查询服务，修复并经 Core 发布后可再次同步。

最新设置缓存于 `processes/.configuration.json`，格式版本 1、读取上限 32 KiB；不可变配置历史仍归 Core。缓存损坏或版本不兼容时保留文件并阻止新启动。保存结果不确定时同样暂停新启动，后续同步成功再恢复。

新 `state.json` 使用版本 2，启动前保存 `configuration`；pipe 与 PTY 采集器固定其中的输出限额，PTY 的 `launch.json` 同时保存该绑定。`process inspect` 可查每条命令实际使用的设置。旧版本 1 继续读取，缺失配置保持未知，不用当前值补写。进程恢复仍查询原身份，不重新派发命令。

## 模型工具

`exec_command` 接受 `command`、可选 `cwd`、`pty`、`cols/rows`、`wait_ms` 和 `max_output_bytes`。cwd 默认继承调用 Agent，相对路径以该目录为基准；pty 默认 false，尺寸只适用于 PTY。`write_stdin` 接受 `process_id`、`chars`、`close_stdin`、各 stream 的 `offsets`，以及相同的等待和输出参数。空输入只等待并读取。

两者默认等待 1000 ms，范围 0–30000 ms；每个 stream 默认读取 32 KiB，最大 64 KiB。结果包含 process ID、状态、可用退出结果，以及各 stream 的 `text/next/total/gap`。下次从 `next` 继续读取。模型收到转义后的文字，原始字节仍保存在 runner。

完整响应校验后，core 保存 `tool.started` 和稳定操作 ID，再在执行前保存 `tool.dispatching`。调用记录 schema 2 据此区分准备和派发；旧版缺少派发标记时按未知处理。启动和续写以原 ID 去重。读回失败保留 process ID；已确认的部分写入报告 `written`，未确认时不填零。

普通 stop 后，已确认启动/输入的进程继续；当前读取取消，结果标 `observation_interrupted:true`，保留 process ID、已读输出及最后观察状态。后续先 inspect 再继续。启动/输入尚未确认或输出记录损坏时，仍保留错误或待核实状态。模型工具 descriptor 版本为 2。

进程工具的只读 `Recover` 接口查询原 process ID/启动参数，或原输入控制记录/参数指纹。pending、缺失、损坏或不符保持待核实；completed 输入返回原 written 和错误，后续读取失败标观察中断。调度器将核实结果与工具消息原子提交，再继续原 Task；核实过程不调用 Start/Input。

显式树停止允许用 runner 的终止证据核实未知 exec：结果附 `stop.operation_id` 与 `stop.state`（`stopped/not_started`），错误为 `task_cancelled`。未启动的调用不再执行；已终止的原进程保留输出与最后观察状态，退出码可继续未知。此证据只解决命令取消，不替代 stdin 或 patch 的原执行记录。

## 输入与控制

```sh
./bin/zende --profile dev --request-id input-demo process exec 'cat'
printf 'hello\n' | ./bin/zende --profile dev --request-id input-once process stdin input-demo --file - --close
./bin/zende --profile dev process control input-demo input-once
./bin/zende --profile dev process resize terminal-demo --cols 100 --rows 32
./bin/zende --profile dev process cancel terminal-demo
./bin/zende --profile dev process terminate terminal-demo
./bin/zende --profile dev process terminate terminal-demo --force
```

`stdin` 接受文本参数或 `--file` 二进制字节，单次最多 64 KiB，不自动加换行。pipe 写入最多等待 2 秒；部分写入通过错误和操作记录中的 `written` 报告。`--close` 关闭 pipe 的写端。PTY 没有半关闭：canonical mode 下发送当前 VEOF 字符并禁用后续输入；raw mode 或禁用 EOF 时明确报错。

`resize` 只调整指定 PTY，并查询内核尺寸确认生效。`cancel` 发送 SIGINT；`terminate` 默认 SIGTERM，`--force` 使用 SIGKILL。pipe 针对已核实的原进程组，PTY 还覆盖该终端 POSIX session 内的 job-control 进程组。主动脱离原组/session 的程序不属于这次信号目标。返回 `cancel_signal` 表示信号已请求，实际结束仍以 `inspect` / `wait` 的退出结果为准。

同一进程的输入和控制串行执行。`--request-id` 在该进程内持久去重；相同 ID 和参数返回原操作结果，查询当前状态用 `inspect`。派发前记录意图，结果未知时保留 pending，重试或换 ID 都不重复发送。此时用 `process control` 检查原操作；终止入口仍可用。

## 树级停止底层接口

core 通过内部 `process.stop.freeze/advance/inspect/release` 执行 `agent stop --tree`。普通 stop 和 detach 保持原行为。

freeze 按稳定操作 ID 保存 Agent 范围、已有进程和已派发调用 ID。捕获范围与启动串行，已进入 runner 的 stdin/resize RPC 先落定；冻结后拒绝范围内的新启动、stdin 和 resize，已完成重试仍返回原回执。未到达 runner 的调用保存为 `not_started`，释放冻结后也不能用该 ID 执行；其他 Agent 不受影响。

advance 核实 boot ID、启动时间及原进程组/PTY session，保存仍存活的成员身份，再通过 pidfd 请求 SIGKILL。只有核实已无活动成员才返回 `stopped` 并释放进程容量；这不代表输出完整、Task 已落定或外部效果已回滚，也不补造崩溃 pipe 的退出码。身份缺失、内核查询失败或无法终止时保持 `frozen`，成员列出 pending/unknown 和失败原因。原输出和已退出的 tmux pane 保留。

Linux 6.9 起可通过 pidfd 向原进程组发信号，覆盖扫描后派生的同组子进程。旧内核退回已核实成员的逐个 pidfd 终止；遗留成员无法确认时继续等待，不按裸 PID 猜测目标。范围与普通 terminate 相同：pipe 的原进程组、PTY 的原 POSIX session；主动脱离该范围的守护进程不在其中。实现依据 [Linux pidfd 接口](https://man7.org/linux/man-pages/man2/pidfd_send_signal.2.html)。这里不使用 SIGSTOP：tmux 会主动恢复被暂停的 pane，见 [tmux 3.3a 的 server_child_stopped](https://github.com/tmux/tmux/blob/3.3a/server.c)。

`stopped` 仍保持启动冻结。core 核实 Task 与 worktree 落定后 release，随后在事务中复查并解除 Agent 冻结；中途崩溃则继续原停止操作。旧操作重试不捕获后来的工作。记录保存于 `processes/.stops/`，runner 重启后恢复；记录损坏或保存结果不明时阻止新进程效果，保留原文件及进程查询。每项停止最多 10000 个 Agent、10000 个待到达调用和 10000 个进程；单个进程的内核成员同限，记录受 IPC 帧预算约束。

## 终端选择与接管

`process list [--agent AGENT --after ID --limit N]` 返回按 ID 排序的进程页，包含已结束进程；默认 20 条，上限 100，响应还受协议帧大小限制。`next` 非空时用它继续。TUI 的 `/terminal`、`/logs` 默认只列当前 Agent 的进程，方向键选择、Enter 打开、n 下一页、Esc 或 `/back` 返回。

`/logs PROCESS [stdout|stderr|pty]` 只读；`/terminal PROCESS` 选择窗口尺寸控制目标，仅调整该活动 PTY。pipe 和其他 PTY 尺寸保持原样。`/resize COLS ROWS` 改为固定尺寸；重新选择 `/terminal` 恢复随窗口调整。resize 结果未知时保留原操作，裸 `/resize` 重试同一 ID，新的尺寸请求和接管暂缓。

日志单次读取 32 KiB，显示缓存保留末尾 64 KiB；控制字节转义，裁切和缺失字节区间可见，PgUp/PgDn 浏览。完整已保存输出仍通过 `process output` 读取。日志视图中的输入发送给 Agent，不写入进程 stdin。

`/attach [PROCESS]` 或交互终端中的 `process attach PROCESS` 核实原 tmux 身份后附着。TUI 暂时交出终端，键盘直接进入原 PTY，Ctrl+B 再按 d 返回；返回后恢复 TUI、刷新进程并从原事件游标补发。接管期间原始按键由 tmux 处理，输出继续采集；按键不生成逐次 RPC 输入记录。CLI attach 拒绝 pipe、非 TTY 和 `--json`。

接管只启动短命 tmux 客户端，退出不终止原 pane。窗口变化通过 runner 调整同一目标，未知 resize 使接管客户端退出并留下可查询记录。接管和日志查看均可与后台 Agent 同时运行，用户应避免与模型同时给同一交互程序输入。

## 保存

进程文件位于 profile 状态目录的 `processes/<process_id>/`。`state.json` 保存版本化状态；pipe 保存 stdout/stderr，PTY 保存合并的 `pty` 原始字节。进程身份包含 runner 实例、PID、boot ID 与内核启动时间；PTY 还绑定 tmux socket、server 生命周期、session、pane 和随机标识。

PTY 启动时先保存意图，创建等待放行的 pane，再由 `pipe-pane` 启动独立采集进程。采集文件和元数据落盘后才放行命令。采集排空后保存 `capture.json`；退出码或信号优先来自已核实的 tmux pane。pane 已关闭但退出字段为空时，可读取 Linux zombie 的原 wait status，须同时匹配保存的 pane、PID、父进程、boot ID 和启动时间。持续 5 秒仍无法核实则标未知，不按请求过的信号推测退出。屏幕快照不参与输出完整性判断。

输出 JSON 的 `data` 为 base64 原始字节，`text` 是普通日志投影：保留文字、换行和 Tab，转义 ESC、OSC、其他控制字符及非法 UTF-8 字节。普通界面使用 `text`，不直接打印解码后的 `data`；真实终端接管另行处理。`next` 是下一次字节游标，单次读取上限 1 MiB。分片边缘的不完整 UTF-8 字节也会转义。

每个进程默认保存最多 64 MiB，pipe 的 stdout/stderr 共用额度；超出后继续排空，`gap.from/to` 表示缺失区间。读取已损坏或缺失的保存范围返回 `output_missing`。runner 的读取缓存上限为 4 MiB、256 块；每次读取先核对文件身份、大小及修改信息。

## 输出保留

```sh
./bin/zende --profile dev process retention
./bin/zende --profile dev --request-id pin-demo process pin process-demo --reference note-demo
./bin/zende --profile dev --request-id unpin-demo process unpin process-demo --reference note-demo
./bin/zende --profile dev process rotate --retain-bytes 67108864 --dry-run
./bin/zende --profile dev --request-id rotation-demo process rotate --retain-bytes 67108864 --yes
./bin/zende --profile dev process rotation rotation-demo
```

活动、unknown、lost 进程及仍有引用的输出均受保护。带 `task_id` 的进程自动持有 `task:<task_id>` 引用；命令退出不会释放它。Task 生命周期由 core 管理，后续接入时须在任务完成且不再需要原输出后显式释放。历史引用使用 `--kind history --owner VIEW`；默认手动引用的 owner 为 `user`。释放时必须匹配 reference、kind 和 owner。`inspect` 显示全部引用，`retention` 返回受保护/可释放字节及限额。

轮转是显式操作，按创建时间从旧到新删除整个进程的输出文件，保留命令、身份、退出结果和引用记录。目标预算低于受保护字节时，在删除前报错。`--dry-run` 只预览；实际执行重新检查引用，`--yes` 确认删除。删除不可恢复，读取时返回显式 gap，不能重新 pin 已释放的内容。

轮转先保存目标清单，再为每个进程保存释放标记后删除固定输出文件。中断后用相同 `--request-id` 和预算重试，只完成原清单，不纳入后来产生的输出。因此预算针对规划时的输出，重试后的总占用可能高于它。当前没有自动全局清理；默认值与长期压力验证归第 9.6 项。

## 恢复边界

core 重启后接回原 runner。runner 被终止后，tmux 与独立采集继续工作，新 runner 从保存的身份和采集记录接回；已完成命令返回原退出结果。server 生命周期或采集身份无法核实时返回 unknown，保留已有输出。

pipe 的文件描述符由 runner 持有；runner 自身退出后无法重新获得。原进程身份仍匹配，或启动意图已保存但 PID 尚未保存时，返回 unknown；已确认原进程不存在时返回 lost。两者都保留输出和原 process ID，不重跑，也不伪造退出码。仍能核实身份的 pipe 可用 `terminate` 停止；stdin 无法接回。退出结果已保存的进程直接返回原结果。

单个进程的元数据缺失、损坏、身份不符或版本不兼容时，该进程显示 unknown，并说明原文件保留；其他进程仍可恢复。该 ID 不能重新启动，控制与输出查询明确报错。已有输出范围缺失时仍保留原字节数，读取报 `output_missing`，不缩短记录掩盖损坏。

遇到 unknown，先查原身份、操作记录及命令产生的文件。`agent recovery` 查看待核实调用，`agent recover` 请求重新查询原操作；确认前 Agent 队列保持等待，换调用 ID 或提交新输入都不能重放。

| 情况 | 查询结果及后续 |
| --- | --- |
| core 崩溃，runner 存活 | 新 core 接回原 runner；命令和输出继续 |
| runner 崩溃，PTY/采集存活 | 新 runner 接回原 pane、字节游标和退出结果 |
| runner 崩溃，pipe 存活 | unknown；可读已落盘前缀，不能继续 stdin；确认后可终止原组 |
| pipe 原身份已不存在 | lost；保留历史，退出码未知 |
| 仅保存了启动意图 | unknown；保留原 ID，不自动派发 |
| PTY 采集器或 server 丢失 | unknown；保留已确认字节，不能据屏幕快照推断完整输出 |
| 采集计数倒退或非法 | unknown；保留先前元数据，不接受损坏计数 |
| 已保存退出结果 | 返回原结果，不依赖原 PID 仍存在 |

可复现的 V07/V13 进程验证：

```sh
go test ./internal/cli -run TestDetachedCoreBinary -count=1
go test ./internal/process -run 'TestRecovery|TestPipeRecovery|TestPTYCaptureAndServerLoss|TestControlPending|TestSignalCancel|TestPTYInputResize' -count=1
```

第一条使用临时构建的真实二进制和隔离 XDG，分别终止测试 core/runner；第二条覆盖启动窗口、损坏记录、采集故障和控制。启动窗口使用确定性落盘夹具，正常运行时崩溃使用真实 SIGKILL。测试只清理自己创建的进程与 tmux socket。

Agent 层恢复由 `TestRecoveryRejoinsRunningPipeAndPTY` 和 CLI 的 `TestBinaryRecoversOriginalRunningToolAfterCoreSIGKILL` 覆盖：保存原调用，重开 core，接回原进程并继续输入，副作用仅一次。`TestRealTerminalHandoffTargetsOnePTYAndReplaysEvents` 验证两个真实 PTY 与一个 pipe 下的定向 resize、接管输入、返回补发和终端恢复。模型均为本地夹具。

实现依据：[tmux 3.3a 输出排空判断](https://github.com/tmux/tmux/blob/3.3a/window.c)、[pane 退出处理](https://github.com/tmux/tmux/blob/3.3a/server-fn.c)。该版本在已退出 pane 上拒绝 `pipe-pane` 操作；采集器核实退出状态后排空自己的 socket，不等待永远不来的 pipe EOF。

Linux zombie 的身份和退出字段依据[内核 `/proc` 文档](https://docs.kernel.org/filesystems/proc.html)：`stat` 的 `exit_code` 使用 waitpid 状态格式。此路径只读内核记录，不向 tmux 或原命令发送恢复信号。

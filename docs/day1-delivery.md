# Day1 交付状态

2026-10-17，功能源码基线 `6c5b367`。本文对应 OpenSpec 11.3，覆盖八项能力的全部 89 个 Scenario。源码入口用于定位实现，不代替场景验收；完整 Day1 尚未验收。

## 已有证据

| 范围 | 记录 | 边界 |
| --- | --- | --- |
| 当前命令产物 | `6c5b367` 经安装脚本构建并安装 | 编译通过，不代表运行行为通过；未单独构建插件示例 |
| 本机安装 | `~/.local/bin/zende` 指向保留版本目录 | 安装保留旧二进制；回退入口未实际执行 |
| 空配置服务 | default Core 更新至 `6c5b367`，runner 保持 `c2f2f96`，配置同步为 current | 没有模型连接、Agent 或活动命令；不是 TUI 接回或带任务升级验收 |
| Core 停止与更新 | 按原实例身份停止自建空闲 Core，启动新版；runner 未重启 | 未覆盖忙碌、响应丢失、替代实例及活动进程场景 |
| 数据库升级 | 空配置库从 schema 25 升到 26，日志记录迁移前副本 | 没有恢复副本，也未覆盖有任务数据、迁移失败及损坏场景 |
| 早期功能 | [历史验证记录](verification.md)保存 2026-10-14 至 16 的局部测试、夹具及故障记录 | 只适用于各节注明的版本和范围，不能推广为当前版本全量通过 |

当前安装目录为 `~/.local/share/zende/releases/20261017T094622Z-6c5b36776dab-Vsh2zGpF/`；数据库、日志位于 `~/.local/state/zende/default/`。模型配置尚未创建，未读取或使用用户凭据。实际部署过程见[升级记录](verification.md#2026-10-17重试实现部署与-schema-升级)。

2026-10-16 起不新增、不运行测试，也不以冒烟、验收脚本或读者测试替代。下面各场景在当前版本均保留为**未完整验收**；已有局部证据单独说明。

## 1. 持久运行时：14 个场景

[完整规格](../openspec/changes/build-day1-harness/specs/durable-agent-runtime/spec.md)

| 场景 | 主要实现入口 |
| --- | --- |
| 关闭 TUI 后接回同一任务；持续会话接受第二项工作 | [独立服务与 CLI](../internal/cli/cli.go)、[任务循环](../internal/runtime/loop.go)、[客户端重连](../internal/render/client.go) |
| 提交超时后重试；核心离线时提交 | [请求接受事务](../internal/store/store.go)、[协议客户端](../internal/protocol/ipc.go) |
| 停止父 Agent 时子任务继续；不可立即取消的操作；显式恢复暂停会话 | [停止/恢复](../internal/store/control.go)、[模型循环](../internal/runtime/loop.go)、[树级停止](../internal/runtime/tree_stop.go) |
| 工具派发前或运行中崩溃；机器重启导致进程丢失；操作生效后结果未落盘 | [恢复调度](../internal/runtime/recover.go)、[恢复日志](../internal/runtime/recovery_journal.go)、[模型重试额度](../internal/runtime/model_retry.go)、[进程身份恢复](../internal/process/recovery.go) |
| 状态查询期间继续产生事件 | [快照与事件存储](../internal/store/store.go)、[日志读取](../internal/store/journal.go) |
| 达到运行限额 | [Core 准入](../internal/runtime/core.go)、[进程管理](../internal/process/manager.go) |
| 复制有后台工作的会话；停止失败或复制提交时崩溃 | [复制协调](../internal/runtime/session_copy.go)、[复制意图](../internal/store/session_copy.go)、[事务发布](../internal/store/session_copy_publish.go) |

历史记录覆盖基础接受、停止/恢复、进程身份及部分未知副作用。新增复制、恢复依赖查询、插件待核实屏障和持久模型重试未测试；本机部署只覆盖空配置服务生命周期及一次升级。

## 2. 多 Agent 协作：13 个场景

[完整规格](../openspec/changes/build-day1-harness/specs/agent-collaboration/spec.md)

| 场景 | 主要实现入口 |
| --- | --- |
| 模型主动委派任务；协作命令连接中断 | [协作 CLI](../internal/cli/collaboration.go)、[spawn](../internal/runtime/spawn.go)、[接受与来源](../internal/store/spawn.go) |
| 无需先搜索核心协作命令；系统和工具说明与能力同步 | [条件式提示词](../internal/runtime/prompt.go)、[CLI 常驻说明](../internal/cli/collaboration.go)、[能力变更通知](../internal/runtime/resource_notice.go) |
| 父上下文已经压缩；自定长度或不继承 | [继承边界](../internal/store/spawn_context.go)、[当前上下文视图](../internal/store/context_view.go) |
| CLI 临时目录与父 cwd 不同；选择 worktree | [spawn 工作区选择](../internal/runtime/spawn.go)、[worktree 持久记录](../internal/store/worktree.go) |
| 投递中途 core 退出；忙碌时分别发送两种消息；暂停时接收加急消息 | [mailbox 与投递事务](../internal/store/mailbox.go)、[调度循环](../internal/runtime/loop.go) |
| 父任务正在 wait 子任务；只输出 CLI 结果不算上下文投递 | [wait 与上下文回填](../internal/runtime/task_result.go)、[结果消息](../internal/store/task_result.go) |

历史记录包含 CLI 归因、上下文继承、worktree、queue/steer、结果回流和树级停止。后续压缩视图、资源版本和复制输入迁移已改动相关链路，未在当前版本重新验收。

## 3. 热加载资源：11 个场景

[完整规格](../openspec/changes/build-day1-harness/specs/live-resources/spec.md)

| 场景 | 主要实现入口 |
| --- | --- |
| 项目覆盖用户配置 | [配置合成与来源](../internal/resource/config.go)、[Core 发布](../internal/runtime/config.go)、[不可变配置版本](../internal/store/config.go) |
| 旧调用运行时更新能力；工作中新增 provider 并修改默认模型 | [资源准备/发布](../internal/runtime/resources.go)、[运行选择](../internal/runtime/selection.go)、[请求版本记录](../internal/runtime/loop.go) |
| 搜索后执行外部工具 | [MCP 搜索](../internal/resource/mcp/search.go)、[模型入口](../internal/runtime/mcp_tools.go)、[持久调用](../internal/store/mcp_calls.go) |
| 替换搜索过的工具；目录新增无关工具 | [定义引用](../internal/resource/mcp/catalog.go)、[调用校验](../internal/resource/mcp/call.go) |
| 原生与兼容接入完成同一任务 | [原生搜索选路](../internal/runtime/mcp_native.go)、[Responses 编码](../internal/model/responses/tool_search.go)、[Anthropic 编码](../internal/model/anthropic/tool_search.go) |
| 在当前任务安装 MCP；外部程序分多次写入资源 | [注册与刷新](../internal/runtime/resources.go)、[文件监视](../internal/runtime/watch.go)、[目录发现](../internal/runtime/discovery.go) |
| 当前任务安装和更新 skill；连续安装多项资源 | [资源正文](../internal/resource/package.go)、[请求资源快照](../internal/store/resources.go)、[有界通知](../internal/runtime/resource_notice.go) |

历史 MCP 记录使用本地服务，涵盖 host 搜索、引用、回执和同任务安装；skill 与通知亦有局部记录。新增配置发布、文件监视、目录发现和原生搜索未测试；没有真实模型端点的原生/兼容闭环证据。

## 4. 只追加上下文：7 个场景

[完整规格](../openspec/changes/build-day1-harness/specs/append-only-context/spec.md)

| 场景 | 主要实现入口 |
| --- | --- |
| 后续请求使用新增信息；模型流输出时发布新上下文 | [任务循环](../internal/runtime/loop.go)、[上下文视图发布](../internal/store/context_view.go)、[请求读取](../internal/store/context_query.go) |
| 接近容量上限；主动压缩 | [容量估算](../internal/model/context_budget.go)、[自动压缩](../internal/runtime/compaction.go)、[主动压缩](../internal/runtime/compaction_manual.go) |
| 模型需要摘要中省略的细节 | [原文游标读取](../internal/store/context_query.go)、[CLI 入口](../internal/cli/context.go) |
| 压缩期间收到 steer 或工具结果；摘要生成失败 | [压缩事务](../internal/store/compaction.go)、[视图边界](../internal/store/compaction_plan.go)、[摘要生成与失败记录](../internal/runtime/compaction.go) |

早期事务、上下文继承和原历史保护有局部记录。当前容量估算、自动/主动压缩、并发尾部和摘要失败路径均未测试；估算不是供应商 tokenizer，也不保证请求一定容纳得下。

## 5. TUI 客户端：15 个场景

[完整规格](../openspec/changes/build-day1-harness/specs/tui-client/spec.md)

| 场景 | 主要实现入口 |
| --- | --- |
| 正常启动和退出；同一任务由两种客户端观察；断线期间任务完成 | [TUI 启动](../internal/render/run.go)、[异步客户端](../internal/render/client.go)、[事件投影](../internal/render/transcript.go) |
| 长命令期间操作界面；PTY 输出控制序列；多进程时调整窗口 | [进程面板与目标 resize](../internal/render/process.go)、[显式终端接管](../internal/process/attach_linux.go)、[输出投影](../internal/process/output_cache.go) |
| 窄窗口和空闲界面；错误首次出现和再次出现 | [布局](../internal/render/layout.go)、[绿色主题](../internal/render/theme.go)、[错误状态](../internal/render/issues.go) |
| 完全通过命令操作 | [共用命令注册表](../internal/render/commands.go)、[输入框下方候选](../internal/render/palette.go) |
| 回答提问时粘贴图片 | [组合输入](../internal/render/composer.go)、[回答草稿](../internal/render/questions.go)、[图片导入](../internal/render/images.go) |
| 同厂商多个端点和空配置；浏览后取消或确认当前目录 | [配置模型与目录选择](../internal/render/selection.go)、[Git 状态](../internal/render/git.go) |
| 复制确认取消；确认复制并继续编辑；关闭后接回复制 | [复制确认与草稿映射](../internal/render/session_copy.go)、[复制草稿读取](../internal/store/session_copy_publish.go)、[独立附件映射](../internal/store/session_copy_data.go) |

历史记录包括真实终端生命周期、重连、PTY 接管、极简布局、命令、图片与提问、配置选配。复制流程及草稿恢复随后加入，未测试；尚未用当前用户终端实际接回一次真实模型任务。浏览器原型不作为 TUI 验收证据。

## 6. 终端与 patch：11 个场景

[完整规格](../openspec/changes/build-day1-harness/specs/terminal-execution/spec.md)

| 场景 | 主要实现入口 |
| --- | --- |
| 交互程序与普通命令；tmux 不可用 | [pipe 管理](../internal/process/manager.go)、[tmux PTY](../internal/process/tmux.go) |
| 多次交互并终止进程；暂时没有输出；对 pipe 执行 resize；连续输入与等待超时 | [进程控制](../internal/process/control.go)、[停止与终态](../internal/process/stop.go)、[模型进程工具](../internal/tool/process.go) |
| 命令输出超过单次返回预算 | [采集与游标](../internal/process/manager.go)、[保留和引用](../internal/process/retention.go)、[模型结果投影](../internal/tool/process.go) |
| core 重启后重连命令 | [独立服务](../internal/process/service.go)、[身份恢复](../internal/process/recovery.go)、[工具核实](../internal/tool/process_recovery.go) |
| 自选读取与生成文件 | [exec_command/继续操作](../internal/tool/process.go)、[调用记录](../internal/runtime/calls.go) |
| patch 与并发修改冲突；非 Git 目录修改文件 | [patch 引擎](../internal/patch/engine.go)、[文本匹配](../internal/patch/text.go)、[工具结果与恢复](../internal/tool/patch_recovery.go) |

历史记录使用真实 pipe/tmux 和文件系统，覆盖多数进程原语及 patch；不等于当前全部场景回归。runner 崩溃后的 pipe 无法保证恢复完整监督状态，身份或结果不足时报告 unknown/lost，不重跑命令。

## 7. 独立进程插件：9 个场景

[完整规格](../openspec/changes/build-day1-harness/specs/extension-plugins/spec.md)

| 场景 | 主要实现入口 |
| --- | --- |
| 插件不支持宿主协议 | [manifest](../internal/resource/plugin/manifest.go)、[启动与握手](../internal/resource/plugin/instance_linux.go) |
| 加载五类插件能力 | [能力目录](../internal/resource/plugin/capability_catalog.go)、[模型](../internal/runtime/plugin_models.go)、[资源](../internal/runtime/plugin_resources.go)、[CLI](../internal/cli/resources.go) |
| 插件监听运行事件；Core 订阅插件事件 | [双向传输](../internal/resource/plugin/events.go)、[Core 协调](../internal/runtime/plugin_events.go)、[持久订阅](../internal/store/plugin_subscriptions.go) |
| 插件调用处理中取消 | [调用与进度](../internal/resource/plugin/call.go)、[持久执行](../internal/runtime/plugin_execution.go)、[取消记录](../internal/store/plugin_cancel.go) |
| 流式请求期间升级 provider 插件；插件升级初始化失败 | [实例发布与租约](../internal/runtime/plugins.go)、[模型流](../internal/runtime/plugin_models.go) |
| 插件在副作用发生后退出 | [调用结果核实](../internal/runtime/plugin_calls.go)、[恢复屏障](../internal/runtime/recover.go)、[依赖报告](../internal/store/recovery_dependencies.go) |
| 用户查询插件运行能力 | [实例状态](../internal/resource/plugin/instance_linux.go)、[诊断](../internal/resource/plugin/diagnostics.go)、[查询入口](../internal/cli/resources.go)、[插件运行约束](plugin-authoring.md) |

历史记录包含五类能力、双向订阅、换版及本地插件副作用故障。新增恢复关联与待核实屏障未测试；插件按当前用户权限运行，进程隔离只服务生命周期和故障处理。

## 8. 模型协议与输出：9 个场景

[完整规格](../openspec/changes/build-day1-harness/specs/model-protocols/spec.md)

| 场景 | 主要实现入口 |
| --- | --- |
| 四种协议运行同一工具任务 | [Responses](../internal/model/responses/adapter.go)、[Chat Completions](../internal/model/chatcompletions/adapter.go)、[Anthropic](../internal/model/anthropic/adapter.go)、[Gemini](../internal/model/gemini/adapter.go)、[循环](../internal/runtime/loop.go) |
| 参数跨流片段到达 | [公共响应组装](../internal/model/response.go)、四 adapter 各自的 decoder |
| 切换协议时存在专用续接数据 | [专用历史边界](../internal/model/history.go)、[显式转换](../internal/runtime/context_projection.go)、[选择预览](../internal/runtime/selection_preview.go) |
| 原生类别或显式文本标记；普通结束但未给出分类；标记分片且正文包含伪标记 | [分类与标记解析](../internal/model/output.go)、[分类提示词](../internal/runtime/prompt.go) |
| final 中途断流或重复出现；final 与工具调用冲突 | [分类校验](../internal/model/output.go)、[有限修复与任务终态](../internal/runtime/loop.go) |
| 兼容端点缺少可选字段 | [按型号绑定](../internal/provider/resolver.go)、[Chat 协议](../internal/model/chatcompletions/decoder.go)、[MCP host 路径](../internal/runtime/mcp_tools.go) |

历史 Responses/Chat 和公共分类使用本地 HTTP 或模型夹具；Anthropic、Gemini、新原生搜索、显式公开历史转换和有界 HTTP 重试未测试。当前命令构建包含四 adapter，不能据此声称任何真实端点组合兼容。能力声明与配置矩阵见[四协议对照](development.md#四协议配置与能力对照)。

## 已知边界与未交付的验收

- MCP 四协议都有 host 路径；原生实现为 Responses client tool search、Anthropic deferred，按端点/型号声明启用。Chat 和 Gemini 使用 host，不添加协议不存在的字段。
- 默认严格续接；跨协议不能携带的私有内容会报错。显式 `--context public` 只为后续请求生成公开历史副本，不改写原记录，也不承诺保留私有推理语义。
- 资源监视等待稳定窗口，无法辨别一个暂时合法的文件是否还会续写。建议写完临时文件后 rename；本地准备成功也不证明远端认证或能力可用。
- tmux 是 PTY 依赖；缺失时不退为 pipe。进程、插件和 shell 没有审批或沙箱，均按当前用户权限执行。
- 容量、并发和退避默认值尚未经过长会话负载测量。新 Task 的额外模型请求计数持久保存，HTTP 重试和 Core 中断重建共用额度；部分流、结果不明的连接和插件不做 HTTP 自动重放。原文和输出引用继续保留。
- schema 26 阻止旧调度器忽略模型重试额度；本机升级已保留 schema 25 副本。迁移故障、损坏和恢复未验收。SQLite 副本不包含 runner、配置、工作区或新产生的历史，不能当作整个 profile 的可随意覆盖恢复点。
- V01–V23 的当前版本组合验收、V22 复制故障、V23 TUI 接回和数据回退均未完成。纯测试事项保留原任务，不执行、不标为通过。

首次模型接入仍需用户指定协议、端点、型号与凭据引用，授权前不读取或使用模型凭据。实际部署和恢复操作见[运行说明](development.md)，实现进度以 [OpenSpec 任务](../openspec/changes/build-day1-harness/tasks.md)为准。

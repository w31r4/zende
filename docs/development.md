# 本地开发

CLI 支持持久会话、子 Agent 创建与复用、历史查询、进程工具和 Responses 接入；独立 runner 管理 pipe、tmux PTY 与输出。当前验证使用本地模型夹具，完整 Day1 和部署验收仍在实施。进度见 [OpenSpec 任务](../openspec/changes/build-day1-harness/tasks.md)。

## 构建与测试

```sh
go test ./...
go build -o bin/zende ./cmd/zende
./bin/zende --help
./bin/zende --version
```

项目使用 Go 1.26，`go.mod` 选择工具链并锁定依赖。参考仓库 `.research/` 保持只读。测试使用临时数据库和 Unix socket。

## 模块

| 包 | 职责 |
| --- | --- |
| runtime | Agent、任务、消息、模型循环与恢复 |
| model | 公共模型消息和供应商适配 |
| provider | 将配置中的连接与型号绑定到 adapter，在请求时解析凭据 |
| workspace | 只读目录分页与 Git 状态查询 |
| tool | 工具描述、调用和结果 |
| process | 独立 runner、tmux PTY、pipe 与输出 |
| resource | 配置来源、凭据引用与资源版本 |
| attachment | 图片格式、解码和大小校验；持久字节由 store 保存 |
| protocol | 命令、事件、错误与 Unix socket |
| store | SQLite 事务、追加历史与状态投影 |
| render | Bubble Tea v2 客户端 |
| cli | 短命命令客户端与服务启动入口 |

`TestCoreDoesNotDependOnUI` 检查核心依赖图。UI 外的包不得导入 Bubble Tea 或 render。

## 配置档和目录

每个 `--profile NAME` 有独立配置、状态和 core。profile 仅允许字母、数字、下划线、连字符，首字符为字母或数字。

| 内容 | 默认位置 |
| --- | --- |
| 用户配置 | `$XDG_CONFIG_HOME/zende/NAME/config.yaml`，缺省根目录 `~/.config` |
| 项目配置 | 当前目录 `.zende/config.yaml` |
| 本地覆盖 | 当前目录 `.zende/config.local.yaml` |
| 数据库和日志 | `$XDG_STATE_HOME/zende/NAME/`，缺省根目录 `~/.local/state` |
| 控制 socket | `$XDG_RUNTIME_DIR/zende/<状态目录哈希>/core.sock` |
| runtime 未配置 | 系统临时目录下的 `zende-<uid>/zende/<哈希>/` |

配置优先级：默认值 → 用户 → 项目 → 本地 → 环境 → CLI → 本次任务。map 递归合并，列表整体替换，标量后者覆盖。未知字段、重复键、错误类型、多文档 YAML 和 null 拒绝发布。`config inspect` 给出有效值及 JSON-pointer 来源；读取配置不创建缺失文件。

环境覆盖支持 `ZENDE_MODEL`、`ZENDE_EFFORT`、`ZENDE_CWD`、`ZENDE_SHELL`。CLI/任务层通过配置合成接口接入，具体功能参数在对应功能实现时注册。

模型由连接配置提供，不内置厂商型号清单。以下只有元数据和凭据引用：

```yaml
connections:
  local:
    label: My endpoint
    protocol: chat-completions
    base_url: https://example.invalid/v1
    credential: env:MY_MODEL_KEY
    models:
      - id: my-model
        context_window: 32000
        max_output: 4096
model: local/my-model
```

凭据支持 `env:NAME` 与 `file:/absolute/path`，只在服务连接时读取。检查配置不解析凭据；缺失引用报不可用。URL 不接受用户名密码、查询或 fragment，key 不放在 URL、CLI 参数或普通配置里。

## Core 与 CLI

```sh
./bin/zende --profile dev core start
./bin/zende --profile dev core status
./bin/zende --profile dev --request-id first-task agent create --file TASK.md
./bin/zende --profile dev agent list
./bin/zende --profile dev agent inspect AGENT
./bin/zende --profile dev agent history AGENT --after 0
./bin/zende --profile dev events watch --agent AGENT --after 0
./bin/zende --profile dev operation inspect first-task
```

`core start` 脱离启动终端，日志写入该 profile 的 `core.log`。`core serve` 为前台/监督进程入口，可用 SIGINT/SIGTERM 停止。锁覆盖 socket 清理到服务退出；并发启动只保留一个状态所有者。连接失败时检查 profile、`config inspect` 和 `core.log`，不要删除数据库或重建会话。

写命令支持 `--request-id`。连接中断后用同一 ID 和相同参数重试，或查询 operation；换 ID 会创建另一项工作。每次网络交换有独立 `id`，持久去重使用 `operation_id`。同 ID 配不同参数返回冲突。

普通任务输入用位置参数或 `--file FILE`，`--file -` 从 stdin 读，最大 2 MiB UTF-8。结果为 stdout JSON，事件为 NDJSON；错误走 stderr。退出码 0 表示请求成功，1 表示服务/命令失败，2 表示协议或输入校验失败。CLI 返回 accepted 只表示事务已提交。

本地协议使用 JSON-RPC 2.0、`schema_version: 1`，每行一个对象，单帧上限 8 MiB。普通请求返回一次响应；`events.watch` 先返回确认，再发送 `event` 通知。快照与 cursor 在同一事务中读取，补发从 cursor 之后开始。seq 缺口或超前明确返回 `cursor_gap`。

子 Agent 通过 `agent spawn --task-file FILE` 创建，模型自动携带调用来源；人类命令另需 `--parent AGENT`。创建立即返回身份，`agent submit` 复用同一会话。继承范围、目录默认值、重试和待办见 [Agent 创建与复用](./collaboration.md)。

## 模型目录与工作目录查询

`zende model connections` 列出当前 core 配置的连接，`model list CONNECTION` 返回该连接的型号、思考力度和能力声明。同厂商的连接按 ID 区分，空配置返回空列表；`supported` 只表示 adapter 已实现，当前为 Responses。查询不解析凭据、不访问端点，认证和网络状态在模型请求时检查。

两级目录均支持 `--after NEXT`、`--limit`（默认 20，最多 100）和 `--version VERSION`；跨页或分步选择携带返回版本，配置变化时报 `catalog_changed`。每页最多 1 MiB，超限可减小页数或缩短元数据。返回值只含公开元数据，端点与凭据引用留在连接配置中。

`zende workspace directory AGENT [PATH]` 只列当前目录的子目录，含隐藏目录和指向目录的链接；相对路径按 Agent cwd 解析。返回 `path`、`parent`、`entries`，根目录省略 parent；客户端据此展示 `.` 和 `..`。支持 `--after NEXT`、`--limit`（默认 40，最多 100）。目录为实时观察，翻页期间新增项可在刷新后出现。

路径按字面处理，保留逻辑链接路径，不作 shell 展开。单次扫描最多 100000 项，超限报错后可直接输入子目录路径；非法 UTF-8 名称和失效链接计入 skipped。浏览和查询不会提交 cwd 选择。

`zende workspace git AGENT [PATH]` 返回分支、commit、上游和本地 ahead/behind，以及暂存、未暂存、冲突、未跟踪项数。非 Git 目录返回 `state: none`；错误、超时和过量输出单独报错。未跟踪目录计一项，子模块内部不扫描，上游状态不 fetch。

Git 查询使用 [porcelain v2](https://git-scm.com/docs/git-status#_porcelain_format_version_2) 和 NUL 分隔，关闭可选索引写入及 fsmonitor hook，忽略启动环境中的仓库重定向。默认限时 3 秒，stdout 2 MiB、诊断 16 KiB；错误不回传 Git 原始诊断。模型可通过 exec 调用这些 CLI，文件读取仍可自由使用终端命令。

## 持久选配

`zende agent selection AGENT` 查询已接受的 cwd、模型、力度及 revision；`agent select AGENT --cwd PATH` 切换工作目录，`--model CONNECTION/MODEL --effort VALUE` 选择配置中的型号和力度。可在一条命令中同时设置。省略 model 保留模型设置；选择模型时，空或省略 effort 使用供应商默认力度。

`--expected-revision` 防止覆盖其他客户端的新选择，`--catalog-version` 核对模型目录版本。两者冲突均拒绝保存，重新查询后再选。切换使用稳定 `--request-id` 去重；已接受重试返回原回执，不重新检查相对路径、目录版本或模型能力。公开回执只含选择，端点与凭据引用保存在运行快照中。

接受选择与 `agent.selection` 事件一起提交。当前响应及其工具仍用原设置；下一请求前追加 developer 变更说明，并与 `model.started.settings` 原子保存。旧提示词、消息和工具结果保留。并发选择、上下文变化或 stop 通过事务核对；准备冲突最多重试 8 次，持续变化时报错。

预检检查图片和协议私有数据兼容性，允许工具尚未返回；实际请求须等调用全部落定。认证与网络在发送时检查。不兼容返回明确错误并保留历史；跨模型私有数据迁移仍待 10.6 实现。选择不解除暂停或待核实状态。未配置默认模型时，可显式选配后处理原来 accepted 的任务。

## 数据与恢复边界

SQLite schema 为 14，使用 WAL + FULL synchronous。版本 1→2 增加暂停和任务优先级，2→3 增加附件与引用，3→4 增加提问和回答投递，4→5 增加资源版本与发布记录，5→6 增加输入选中的 skill 版本，6→7 增加 Agent 选配。版本 8 加入上下文视图事件，9 保存 worktree 准备，10 加入 mailbox，11 保存结果投递回执，12 加入整树停止事件与调度保护，13 保存资源准备状态，14 保存 MCP 调用与结果回执。迁移和请求接受各自原子提交，保留历史。数据库约束保护历史、接受记录、附件、任务输入、已接受回答、资源版本与已完成的 MCP 回执；更高版本拒绝打开。

core 启动先将中断任务置为待核实，再按原身份恢复。取消中的任务保留 cancelling 和暂停选择；核实完成后结束取消，不恢复执行。历史、已接受输入和原 Task ID 保留。

`zende agent recovery AGENT [--task TASK]` 只查询恢复记录，默认选择未结束任务，否则选择最新任务。结果含请求 ID、修复次数、调用身份/版本、准备/派发/结果的事件序号和 `next_step`；不返回工具参数或供应商私有数据，也不触发执行。

| 记录 | 恢复判断 |
| --- | --- |
| 模型片段尚未提交为完整响应 | 保留片段，后续请求从完整上下文开始 |
| 完整调用，尚无派发标记 | planned/prepared，使用原工具版本继续 |
| 已派发，结果未提交；或旧版只有调用意图 | dispatched/unknown，先核实原操作 |
| 结果和对应工具消息已原子提交 | completed，复用已有结果 |
| 显式 final 已保存 | finish，按原结果结束任务，无需再次请求模型 |
| 身份、参数、修复次数或结果配对不一致 | 报 recovery_invalid，保留原历史 |

活动任务返回 observe，已结束任务返回 terminal。`next_step` 是查询时的判断；查询本身只读。

调度器每次启动为每个待核实 Task 自动尝试一次。进程工具查询原进程及控制记录，patch 查询独立结果记录。已核实结果与对应工具消息同事务提交；仍有未知调用时继续保持 unknown，新输入只入队。核实状态改变后，用 `zende --request-id CHECK agent recover AGENT [--task TASK]` 请求再次核实；相同 ID 去重。它返回 accepted，实际进度查 recovery/history。

Task 保存启动设置和提示词；每次 `model.started` 另存实际 cwd、模型、端点、力度、输出上限和选择版本，格式修复额度在 Task 内固定。恢复先按原请求快照核实、处理旧调用，再为下一请求应用待生效选择；当前目录移除连接也不改变保存的绑定。快照只含凭据引用，请求时读取当前值。设置缺失、版本不兼容或待派发工具版本变化时保留待核实。配置自动热更新由第 7 组接入。

未完整提交的模型流保留原始片段，下次请求使用已提交上下文；已派发的格式修复计入原额度，重启不刷新次数。恢复提交使用事件游标核对并发 stop/新输入，冲突时重读，最多 4 次；持续冲突可显式 recover。

## 停止与恢复

`zende agent stop AGENT` 持久保存停止请求，取消当前模型请求或可取消调用，并设置人工暂停。通过 `agent inspect` 查看当前状态，`agent history` 查看关联任务和工具调用。命令返回 accepted 只表示请求已保存。

| 情况 | 结果 |
| --- | --- |
| 当前任务仍在处理调用 | Task 为 cancelling；保留已执行效果，等待真实结果，不提前报告停止完成 |
| 调用已落定 | 保存结果，Task 变为 cancelled；剩余未派发调用补上“未执行”的工具结果，原上下文保持合法 |
| 有操作结果未知 | Task 保持 cancelling，Agent 为 unknown；resume、新输入和更换调用 ID 均不能跳过核实 |
| 当前没有运行任务 | Agent 暂停；已接受队列保留，已完成任务终态不变 |
| 已确认启动的后台进程 | 停止观察并保存 process ID，进程继续；终止进程需显式 `process cancel/terminate` |

`zende agent resume AGENT` 解除人工暂停，按既定顺序领取已接受任务；队列为空时创建新续接任务。暂停时用 `agent submit` 提交新输入，该输入优先开始，原队列随后处理。取消尚未落定时可以接受恢复意图，但新任务要等旧调用落定；旧 cancelled 任务始终保持原身份和终态。

stop/resume 使用稳定 `--request-id` 去重。重试旧 stop 只返回原接受记录，不停止后来任务。停止与 final 通过数据库事务排序：先提交 final 则保留 completed，先提交 stop 则取消优先，均保留人工暂停。

`agent.paused`、`task.cancelling`、`task.cancelled`、`agent.resumed` 为追加事件，`paused` 单独记录用户选择。取消过程中崩溃或效果不明时出现 `task.cancellation_pending`，原调用记录保留。`agent stop AGENT --tree` 显式停止整树，暂停时收到的 mailbox 消息继续保存；范围、核实与恢复见 [停止与恢复](./collaboration.md#停止与恢复)。

## Agent loop

后台 core 调度 accepted 任务。同一 Agent 的数据库领取事务只允许一条循环，其他输入保持排队；不同 Agent 可并行。任务开始才把输入追加到模型上下文，后续模型请求读取已提交的完整消息。循环生命周期属于 core，CLI/TUI 断开不会取消它。

任务启动时保存设置和提示词；每次请求记录上下文游标、工具定义/版本与请求身份。adapter 依次报告消息、文本、参数片段、调用结束、用量和响应结束；运行时保存片段，待整个响应完整并校验后执行工具。工具结果与对应上下文消息同事务提交，普通工具错误返回模型继续处理。结果未知或落盘失败时保留任务为 unknown；排队输入不绕过此状态。

`response.end` 只表示响应传输完成。可见输出使用已声明支持的原生 commentary/final，或各占一行的文本标签。原始消息与分类结果分开保存；缺失分类、标签未闭合或 final/tool 冲突进入有界修复。`limits.format_repairs` 默认 1，按 Task 累计，0 表示直接报错。修复请求只接受分类文本，工具调用明确拒绝执行；有效 commentary 可回到正常工具循环。

原始 delta 保留，跨 UTF-8 字节的片段另用 base64 `text_bytes` 保存原字节。单块/单个工具结果上限 1 MiB，响应组装上限 4 MiB、128 块，分类结果也限 128 块。具体标记与历史事件见 [输出分类](./prompts.md#输出分类)。

core 注册 `exec_command`、`write_stdin`、`apply_patch`。进程调用经 Unix socket 交给 runner，文本改动由 [patch](./patch.md) 应用。没有默认模型且 Agent 未选配时，新任务保持 accepted；已有任务可按保存的绑定恢复。`model_loop: running` 表示调度器运行，连通性、凭据和具体型号能力在请求时检查；没有模型解析器则为 `unavailable`。

## 本地 CLI 链路验证

```sh
go test ./internal/cli -run 'TestConfiguredResponsesBinaryExecutesPatch|TestBinaryRecoversOriginalRunningToolAfterCoreSIGKILL' -count=1 -v
```

测试临时构建真实二进制，在隔离 XDG 下启动 core/runner，使用本地 HTTP 模型夹具。第一条链路执行 patch、回填结果并保存 final。

第二条分别跑 pipe 与 tmux PTY：模型启动长命令 → 打开 TUI 并 Ctrl+C detach → 原 Task 继续 → 新 TUI 接回 → SIGKILL 测试 core → TUI 显示离线、原进程继续 → 移除临时连接配置并重启 core → 按保存绑定继续原进程输入 → 原 Task 完成、TUI 自动补齐 final。检查 Agent/Task/process/PID 身份、只有一份任务、命令效果一次、模型请求三次，以及两次 TUI 退出恢复 termios。`-v` 输出各阶段和测试身份；结束后清理测试服务、private tmux socket 与临时目录。

这条演示只终止 core，runner 存活。runner 自身崩溃时，pipe 无法接回原描述符，PTY 则需核实 server/pane/采集记录。原工具结果无法核实时保留 unknown，TUI 重连不解除此状态；凭据引用仍须有效。具体错误查 `agent recovery` 和测试配置档的 `core.log` / `runner.log`，不要重新提交原任务。

模型夹具验证接线和恢复，不代表真实模型兼容性或本机安装验收。真实模型接入仍使用用户授权的端点和凭据引用。

## TUI 客户端

已有 Agent 可用 `zende --profile dev tui AGENT` 打开 alt screen。core/runner 沿用前面的独立服务；Ctrl+C 或 `/detach` 只退出客户端。Enter 提交输入，Alt+Enter 换行，`/help` 按需显示命令。`/stop`、`/resume`、`/recover` 调用同一 core 接口；接收状态不明时 `/retry` 复用原请求 ID 和参数，新输入保留。

输入、事件、窗口尺寸只更新本地状态，网络操作通过 Bubble Tea Cmd 执行。终端清理由框架负责；关闭客户端取消订阅和请求等待，已接受工作继续运行。`tui --plain AGENT`、`--json` 或 stdout 重定向时只输出 JSON 快照；持续线性输出用 `events watch`，提交用 `agent submit`。

主界面零边框，常驻输入两行、底部状态一行；空闲时不占用后台或警告入口。80×24、60 列、48×16 按单元格裁切，长路径保留末尾，更小窗口显示尺寸提示。底部显示模型、cwd 和 Git 分支，工作树有改动时附 `*`；默认模型从已保存请求的公开型号补齐，端点快照保持私有。

命令、候选和点击共用 registry，`/help` 从中生成。输入 `/` 后，最多四行候选在输入框下方展开，状态栏仍在底部；方向键选择，Tab 补全，Enter 执行，Esc 收起。草稿末尾留空格再输入命令，可临时操作并返回原草稿；带参数命令用 Tab 补全后继续填写，校验失败时保留命令，Esc 返回草稿。参数支持单/双引号和反斜杠转义，仅作分词。粘贴保留为文本，提交需显式 Enter。

点击模型或 `/model` 依次选择连接、型号、思考力度，最后 Enter 确认；同名连接显示各自 ID。目录来自 core 配置，空配置显示空列表，尚未实现的协议标 unavailable；default 力度表示供应商默认值。`/effort [VALUE]` 与 `/select` 对应力度和确认项。整个选择携带初始 revision 与目录版本，冲突时重新打开选择页；接受未知时 `/retry` 复用原请求。

点击 cwd 或 `/cd [PATH]` 打开单页目录。上下键或 j/k 选择，右键或 l 进入，左键或 h 返回父目录；`.` 行用 Enter 确认当前位置，`..` 行进入父目录。`/cd 'PATH'` 支持含空格的路径，页内相对路径按浏览位置解析；`/next` 翻页。浏览保持运行设置，Esc 取消并返回原输入或回答，文字、图片和 skill 保留。选择页内 `/` 与 `$` 临时打开组合输入框；粘贴返回原输入、保留为文本。路径和元数据中的控制字符只在显示时转义。

点击分支或 `/git` 查看本地 Git 详情。查询结束后等 10 秒再刷新，同目录最多一个请求；目录变化立即清除旧观察，晚到回复丢弃。非 Git 目录隐藏分支，详情显示原因；错误按需进入警告区，上游计数缺失显示 `?`。`/refresh` 可手动刷新。

主题沿用 `.design/tui-preview/theme.js`：正文 `#3BE13B`、背景 `#1A1A1A`，输入面与次要绿色按原型混色。256 色终端作颜色转换；16 色终端保留默认前景，避免把用户已改为粉色的 ANSI green 当成绿色。`NO_COLOR` 使用无色样式，`tui --ascii AGENT` 用普通字符图标。光标与选区保留白色或终端默认值。组合输入组件独立持有草稿，Bubbles textarea 处理编辑和粘贴。

`/background` 或状态栏后台计数打开当前 Agent 树的工作，数据来自只读 `agent activity AGENT`：待处理 Task、活跃子 Agent，以及属于该树的 pipe/tmux。空闲项隐藏；未知或 lost 进程保留供核实。每次查询结束后等 3 秒再刷新，同时最多一个请求。最多显示 1000 项，截断标 `+`，数据源不可用或旧观察标 `?`。核心状态与进程查询分开观察，不提供跨服务原子快照。

`/issues` 或状态栏警告计数展开/收起错误，Esc 收起。同一来源/错误类型首次展开，重复发生只保留状态计数；用户主动展开后保持打开，问题解决后移除入口。客户端记住 64 类错误，单条显示最多 512 列、详情最多五行；这些显示限制不改核心历史。

终端恢复由 Bubble Tea 管理；项目内两个依赖补丁分别修复输入描述符释放竞争、恢复终端时的尺寸竞争，来源和范围见 `third_party/*/ZENDE.md`。

对话按核心事件显示输入、工具状态和 commentary/final。`~` 表示过程说明，`=` 表示 final；尚未验证的输出标记 streaming，无效或中断后保留对应状态。最终文本确认与 Task 完成分别取自核心事件。`/commentary` 切换整体折叠；加空格后按正文搜索并选择单条，效果与点击该条相同。`/commentary ENTRY` 也接受历史中的条目标识。PgUp/PgDn 或滚轮浏览当前窗口，折叠只改变显示。

首次连接从事件起点回放，断线后按最后成功处理的 seq 自动补发、去重。重试间隔从 250 ms 增至最多 5 秒；连通的空闲订阅也能确认在线。旧连接消息不影响新订阅，重连保留草稿和未确认请求；只有显式 `/retry` 才重试原写请求。历史缺口、身份错误或记录超限停止自动重连并报错，`/refresh` 或点击 offline 从原位置重试，不跳到新快照。

显示缓存最多 512 条、4 MiB 正文及图片/skill 引用，单条正文保留末尾 64 KiB；裁切和窗口淘汰有标记，完整历史仍由 `agent history` 查询。PgUp 最多回看当前窗口的 10000 行。

`/logs` 选择只读日志，`/terminal` 选择随窗口调整尺寸的 PTY；两者也接受 process ID。`/next` 翻当前列表页，`/back` 返回对话；关闭选配页时回到原输入或回答。`/attach [PROCESS]` 显式接管真实终端，Ctrl+B 再按 d 返回。返回后刷新进程并从原事件游标补发；普通输入始终提交给 Agent。尺寸和控制边界见 [终端选择与接管](./processes.md#终端选择与接管)。

## 图片输入

CLI 可先保存图片，再把引用随任务提交：

```sh
zende --request-id image-1 attachment import AGENT '/absolute/path/image.png'
zende attachment list AGENT --limit 20
zende attachment inspect AGENT IMAGE
zende attachment read AGENT IMAGE
zende --request-id message-1 agent submit AGENT '看一下这张图' --image IMAGE
```

导入返回 `attachment.attachment_id`，相对路径按 Agent cwd 解析。支持 PNG/JPEG 普通文件及指向普通文件的符号链接；先检查字节数和尺寸，再完整解码校验。原始字节、SHA-256 和元数据保存到 profile 数据库，任务及上下文只保存引用。`read` 经完整性检查后输出 base64 JSON；普通历史、列表和快照不展开图片字节。

单图上限 4 MiB、16,777,216 像素，每条输入最多 8 张；每次模型请求中的图片总计最多 12 MiB，同一引用出现多次也重复计入。超限明确报错，原附件保留。这些是当前 harness 限额，供应商还可能有自己的容量限制。

`--image` 可重复，允许纯图片输入。所选型号须配置 `images: true` 且 adapter 支持图片；能力不足时提交报错，附件保留，任务不创建。已接受的导入与提交按原请求 ID 去重，即使源文件删除或当前模型配置改变，也返回原接受结果。已导入但尚未提交的附件仍可用 `list` 和 `read` 查询。

TUI 用 `/image 'PATH'` 导入，`/images` 查看已保存图片；`/image-use ` 和 `/image-remove ` 后的空格展开候选，可用方向键与 Enter 选择。也可直接输入 IMAGE ID。在已有文字后输入这些命令时，先用 Tab 补全进入临时命令输入，原草稿与附件保留。图片行只在选中附件或导入未结束时显示；移除仅取消本次选择，保存的数据仍可重新选择。

导入结果未知时，`/image` 不带参数重试原请求；确认前暂停发送这份草稿。模型不支持图片时，文字和附件保留；发送接受后只清空未改动的原草稿，晚到的回执保留后续编辑。纯图片消息在历史中显示图片数量。主输入与提问回答共用文件路径导入和附件选择。

Responses 将图片引用解析为保存的字节，发送 `input_image`、base64 `image_url` 和 `detail: auto`，仅接受 user 图片输入；字段按 [官方图片输入文档](https://developers.openai.com/api/docs/guides/images-vision) 核对，并参考 Pi `packages/ai/src/providers/openai-responses-shared.ts`。协议验收使用本地 HTTP 夹具，TUI 另经真实 Linux PTY 和 Unix socket 验证。

## 模型提问

模型通过 CLI 保存问题，用户可从另一客户端回答：

```sh
zende --request-id question-1 question ask AGENT --task TASK --file QUESTION.md
zende question list AGENT
zende --request-id answer-1 question answer AGENT QUESTION --file ANSWER.md --image IMAGE
zende question inspect AGENT QUESTION
```

`ask` 只接受正在运行且未暂停的 Task，返回 `question_id`。问题最多 8 KiB UTF-8；同一 Agent 最多 8 个待回答或待投递问题。`answer` 接受最多 1 MiB UTF-8、图片引用和 skill 版本，可仅附图片或 skill。图片限额沿用图片输入；能力检查读取最后已发出请求的配置，旧记录回退到 Task 启动配置，失败时问题保持待回答，附件保留。

当前工具全部结束后，Agent 等待全部回答，再按问题创建顺序追加 user 消息。文字、图片和所选 skill 正文一起投递；`context.message` 的 `question_id`、资源引用与投递游标同事务保存，重启和重试只投递一次。等待不占用 IPC 请求；其他 Agent 继续调度。普通输入仍排入任务队列。

`list` 默认只列待回答问题，可用 `--state answered|cancelled|all`、`--after`、`--limit` 查询；列表和快照只带问题元数据，`inspect` 返回回答、图片及 skill 引用。回答可处于已接受但尚未投递状态，`delivered_seq` 标记实际进入上下文的位置。

停止 Task 后拒绝新的回答；取消完成时，未回答的问题变为 cancelled，已接受的回答保留并写入上下文。结果未知的 Task 保留待恢复状态，核实后继续等待或投递。连接结果未知时复用原请求 ID；重试已经接受的回答不会恢复已停止的 Task。

TUI 有待回答问题或保留的回答草稿时，状态栏显示 `? N`，点击或 `/questions` 打开列表。`/answer` 在只有一项时直接进入，否则打开列表；`/answer ` 后按问题正文或 ID 搜索，方向键和 Enter 选择。问题出现在正文区，回答复用两行输入、多行/中文粘贴与图片命令。新问题到达时保留当前焦点。

主草稿与各问题的回答草稿独立，`/back` 返回主输入并保留回答。提交只写入选中的问题；接受时仍在编辑原回答且草稿未变，则返回主输入，否则保留后续编辑和当前焦点。问题已由其他客户端回答或取消时显示关闭状态，原草稿仍可查看，提交明确拒绝。回答草稿最多保留 16 份，满额报错；`/answer-clear [QUESTION]` 显式清除本地草稿，导入或回答结果未确认时拒绝清除，已保存的图片和历史保持。

问题、已接受回答与附件由 core 持久保存，关闭 TUI 后仍可重新打开回答；尚未提交的编辑草稿目前只保存在客户端内存，持久草稿由后续恢复与会话复制任务接入。

## Skill 与 prompt 资源

安装完成后显式发布，模型或用户可在原 Agent 中立即查询：

```sh
zende --request-id install-review resource register AGENT /path/to/review --kind skill
zende resource list AGENT --kind skill --query review
zende resource read AGENT RESOURCE --version VERSION
zende resource read AGENT RESOURCE --version VERSION --path assets/example.txt
zende --request-id refresh-review resource refresh AGENT RESOURCE
zende resource status AGENT RESOURCE
```

skill 接受目录或 `SKILL.md`，prompt 接受单个 Markdown 文件；相对路径按 Agent cwd 解析。入口要求 YAML frontmatter 中的 `name`、`description` 和非空 UTF-8 正文。名称限 64 个小写字母、数字及单连字符，描述限 1024 字符。支持 `disable-model-invocation`，其他元数据作为原文件保存；重复键、别名和错误类型拒绝发布。

发布保存入口正文、包内文件及可执行位，SHA-256 标识内容版本。目录身份由种类和源路径决定；`list` 只返回已发布的名称、描述、来源和版本等元数据，默认 20 项、最多 100 项。每个 Agent 最多记录 512 个来源，含准备中或失败的安装，同一来源更新不另占名额；目录按 profile 隔离。`read --version` 从数据库读原版本，源文件改动或删除后仍可用；文本返回 UTF-8，二进制返回 base64，入口另带文件清单。脚本作为资源保存，执行仍走进程接口。

准备阶段先保存来源与 attempt ID，再读取两次完整文件集合；发布在同一事务内保存版本、更新目录、追加 `resource.published`、完成准备状态和接受回执。较新的尝试或发布会阻止旧候选覆盖，返回 `resource_changed` 后可重新 refresh。同一版本重新校验成功只更新状态，目录 generation 保持不变。

`status.state: ready` 表示已有可用版本；`status.update` 单独显示最近尝试的 `preparing/ready/failed`、请求 ID 和错误。首次安装失败时没有版本，state 为 failed，错误 JSON 的 `data.resource_id` 可直接用于 status/refresh。失败及准备中的内容不进入 list/read；旧版继续可用。资源变更请求 ID 限 1–128 字节，已接受请求的重试返回原回执，不读源文件或覆盖后来状态。

状态变化追加 `resource.update` 事件。新 core 将旧实例未完成的准备标为 interrupted，保留已有版本，等待显式 refresh；当前实例的准备继续。失败诊断使用固定错误类型和说明，不保存解析器原文。

入口限 128 KiB，单文件 1 MiB、包 8 MiB/256 文件，目录深度 16 层、扫描 1024 项；`.git` 跳过。包内文件链接可指向包内普通文件，越界链接、目录链接和特殊文件拒绝。双次读取能发现准备期间的变化；尚未完成但暂时合法的文件仍需安装方控制发布时机，建议完整写入后原子替换。

TUI 输入 `$` 或 `/skill` 打开当前 Agent 的已发布目录，候选在输入下方按名称、描述或来源过滤。方向键选择，Enter 附到草稿，Tab 补全命令；主输入与提问回答共用。多行草稿末尾留空格再输入 `$名称`，选择后返回原文字与图片。粘贴按文本保留，Esc 收起候选后可发送字面量。

选择只改变当前草稿，最多 8 个不同 skill；选中后才显示一行名称，点击或 `/skill-remove` 可移除。目录刷新保留已选版本，换版本须先移除。目录只读元数据、最多 512 项；分页代次变化最多重读 3 次，仍不一致时报错，可用 `/skill` 重试。

CLI 用 `agent submit AGENT --skill RESOURCE@VERSION` 或 `question answer AGENT QUESTION --skill RESOURCE@VERSION`，`--skill` 可重复，也支持纯 skill 输入。提交时 core 核实该 Agent 发布过确切版本，将引用与输入原子保存；任务开始或回答投递时追加该版本正文、来源和版本到 user 消息。`disable-model-invocation` 允许用户显式选择；这不赋予正文 developer/system 权限。刷新和源文件删除不改已接受输入；辅助文件用 `resource read --version` 读取，脚本走进程接口。

资源发布后，下一模型请求追加有界发现通知，按实际目录保存版本；旧 Task 的基础提示词和旧正文保持原样。限额、恢复与读取入口见 [请求级更新](./prompts.md#skillprompt-的请求级更新)。`resource_dirs` 自动发现和目录监视仍在实施。

## MCP 注册、搜索与调用

MCP 复用资源注册、刷新和状态入口；`resource list --kind mcp` 发现服务。Core 管理 stdio 与 Streamable HTTP 连接，将完整目录保存为不可变资源版本。模型只注册 `tool_search`、`tool_invoke` 两个入口，默认上下文不平铺服务工具；CLI 提供搜索及原调用的只读查询。

安装完成后，将非敏感配置写入 YAML 或 JSON 文件：

```yaml
schema_version: 1
name: local-tools
description: Project utilities
transport: stdio
command: /absolute/path/to/mcp-server
args: [--stdio]
env:
  API_KEY: env:MY_SERVICE_KEY
```

stdio 的 `cwd` 可选，默认为配置文件目录；继承 Core 环境，`env` 仅接受 `env:NAME` 或 `file:/absolute/path` 引用。HTTP 配置使用 `transport: streamable-http` 和 `url`，可选 `credential`（Bearer 引用）或 `headers`（请求头到引用的映射）。URL 不接受账号、查询参数和片段，不跟随重定向。不要把密钥放在 command、args、URL 或描述中；解析和保存配置时不读取引用值，连接前才解析。

```sh
zende resource register AGENT /absolute/path/to/mcp.yaml --kind mcp
zende resource status AGENT RESOURCE
zende resource list AGENT --kind mcp
zende mcp search AGENT RESOURCE --query files --limit 8 --max-bytes 65536
zende resource refresh AGENT RESOURCE
```

搜索返回完整 definition、`tool_ref`、`catalog_version`、`resource_version`、发布 `generation` 和可选 `next_cursor`。用相同查询和 `--cursor` 翻页；目录更新后游标失效。服务配置最多 128 KiB，保存在该资源的入口文件中，可按版本读取；目录通过搜索读取。

连接、全页校验和配置复读通过后才发布。失败保留旧连接和旧版，状态区分当前可用性与最近更新失败。变更通知先持久发布，再替换搜索快照；请求回执重试不重连。Core 重启后状态为 unavailable，首次搜索从最后发布的配置重连并校验，磁盘上的未发布修改不参与恢复。关闭 TUI 不关闭连接；Core 正常退出回收连接及 stdio 进程组。

模型可在同一 Task 内执行安装命令，完成后 register/refresh，再用 tool_search 返回的引用调用 tool_invoke。长安装或准备命令返回 running 时，保留 process_id，用 write_stdin 继续等待。首次注册失败，从错误的 data.resource_id 查询 status；补齐依赖后 refresh。更新失败时，status 分别显示上一版 ready 和本次 update.failed。

连接使用 [官方 Go SDK v1.8.0](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.8.0)。协商和首次目录读取成功后才返回，准备限时 30 秒；CLI 的 register/refresh/search 总等待预算为 40 秒，也受调用方更短的取消期限约束。返回后由所有者显式 Close。初始请求结束不关闭连接；连接断开后查询报 unavailable。收到工具变更通知会合并触发刷新，显式 Refresh 总是重新读取，不使用 SDK 的页缓存。

每次刷新先读完所有页，再替换内存快照。循环游标、重复名、SDK 丢弃的非法定义、schema 错误或超限都会拒绝整次更新；旧目录与最近更新错误分开保留。输入和输出 schema 必须为 object，按 [JSON Schema 编译器](https://github.com/santhosh-tekuri/jsonschema/tree/v6.0.3) 校验元 schema 和引用；接受 draft-07、2020-12，省略版本时使用 2020-12。只使用内置元 schema 和当前定义，关闭外部文件与网络加载。

工具引用绑定 server ID、非敏感连接版本和 SDK 可表达的完整定义。新增无关工具不改变原引用，目标变化或删除后在新目录查询旧引用返回 stale_reference。目录内容哈希独立于工具引用，也不等于持久发布 generation。搜索按名称、标题和描述确定性排序，返回完整定义及引用；游标绑定查询和目录版本，改版后须重新搜索。

| 限额 | 当前值 |
| --- | --- |
| 目录准备 | 4096 个工具、128 页，定义合计 16 MiB |
| 单个定义 | 256 KiB，JSON 深度 64；服务端游标 4096 字节 |
| 搜索 | 默认 8 项/64 KiB，最多 50 项/1 MiB；查询 1024 字节 |

搜索字节预算包含整个 JSON 结果；首项放不下时明确要求扩大预算，不截断 schema 或跳过该项。连接诊断使用固定错误，不记录服务端原始错误。

调用底层使用 `LeaseCall` 准备，再由一次性 `Call.Invoke` 发出请求；调用方必须先保存派发记录，并在完成或放弃时释放租约。参数按所选定义校验，拒绝重复 JSON 键和超限输入，传输保留原始参数字节。调用前再次检查引用；开始后的结果仍按原输出 schema 校验。已知工具失败保留 `isError` 和内容；断线、取消后缺少结果、结果超限或不合规均返回 `operation_unknown`，不自动重试。无效但可编码的有界响应随错误返回，供上层保存证据。

结果保留 SDK 支持的文本、图片、音频、内嵌资源、链接、结构化内容及元数据；这是 SDK 解码后的表示，不承诺原始 wire 字节不变。资源链接不会自动读取。参数最多 1 MiB、深度 64，数字最多 1024 字符且指数绝对值不超过 1024；响应最多 16 MiB、1024 个内容块、深度 64。HTTP 参数头限 16 KiB，按固定定义生成；配置不能覆盖 MCP 协议头或 `Mcp-Param-*`。input-required 等需续接的响应暂不支持，也不重新执行调用。

配置替换会停止旧连接接收新租约，已有调用结束后再关闭；Core 退出可强制关闭当前及旧连接。HTTP 取消不保证远端停止或回滚。固定的是本地定义、校验和连接；同一远端服务器自行改变实现时，MCP 没有通用的版本执行保证。

模型调用示例（Agent、Task 和操作 ID 由 host 填入，不接受模型指定）：

```json
{"server_id":"RESOURCE","query":"files","limit":8,"max_bytes":65536}
```

上面传给 `tool_search`；取得完整定义后，将下面的对象传给 `tool_invoke`，其中 arguments 必须满足返回的 inputSchema：

```json
{"server_id":"RESOURCE","tool_ref":"RETURNED_REF","arguments":{"path":"example.txt"}}
```

派发前保存 Agent/Task、参数指纹、发布版本、generation、完整工具定义和目录版本，再执行一次 `tools/call`。同一操作重复进入只读取原回执，参数不同报冲突。调用返回后先保存结果，再把有界回执送回模型；`result.is_error` 表示远端工具报告失败，内容仍保留。原生搜索还检查编码后的模型结果预算，超限要求缩小结果，不截断定义。

完整结果按 SHA-256 保存在数据库同级的 `mcp-results/`，文件 fsync 后才提交回执和事件。普通事件与上下文仅保留回执：最多 8 个部件预览、每项文本最多 1024 字节；`more` 表示还需读取。完整 JSON 包含 SDK 支持的内容和元数据；二进制部件按引用保存，不自动下载资源链接。结果文件持续保留，当前没有自动清理。

```sh
zende mcp inspect AGENT OPERATION
zende mcp result AGENT OPERATION
zende mcp result AGENT OPERATION --part structured
zende mcp result AGENT OPERATION --part 0 --offset 0 --limit 65536
```

`--part` 接受完整 `result`、`structured` 或从 0 开始的内容序号。分页默认 64 KiB，允许 4–256 KiB；按 `next` 继续，直到 `more=false`。文本页保持 UTF-8 边界，图片、音频和内嵌二进制以 JSON base64 返回。每次读取核对完整结果的长度与哈希，缺失、损坏、符号链接或非普通文件明确报错。

Core 重启后，已保存结果可补入原工具结果位置，无须重连 MCP。只保存派发、未保存结果的调用保持 unknown，原任务等待核实，禁止自动换 ID 重试。没有 MCP 派发记录则可确认尚未执行。远端可能已经产生效果，读取回执不会消除这种不确定性；恢复不承诺远端 exactly-once。当前 CLI 没有单独的执行命令，执行入口是模型的 `tool_invoke`。

## 插件启动与协议协商

Core 已接入插件注册、目录持久化和状态查询。调用底层已实现；模型工具接入、CLI 执行、持久调用回执和其余能力由 OpenSpec 8.2–8.5 完成。

```yaml
schema_version: 1
id: local-extension
version: '1.0'
protocol_versions: [1]
command: /absolute/path/to/plugin
args: [--stdio]
capabilities: [tools, commands]
env:
  API_KEY: env:MY_PLUGIN_KEY
```

`id` 为最多 64 字节的小写字母、数字和单连字符；`version` 为最多 128 字节的非空、无空格 ASCII 发布标识。`capabilities` 接受 tools、providers、commands、resources、events，至少一项且不能重复。配置拒绝未知/重复字段、null、别名、错误类型和多文档 YAML。manifest 最多 128 KiB；协议版本列表最多 8 项。

执行文件直接启动，参数不作 shell 展开；命令名按 Core 的 PATH 查找。cwd 默认 manifest 目录，也可指定绝对路径。子进程继承 Core 环境，env 覆盖只接受 `env:NAME` 或 `file:/absolute/path` 引用，值在启动前解析。不要把凭据写进 command、args 或版本字段。准备后复读 manifest，内容变化即关闭候选并报 resource_changed。

stdin/stdout 使用 [JSON-RPC 2.0](https://www.jsonrpc.org/specification)，UTF-8、每行一个对象，末尾换行。宿主发送 `plugin.initialize`，params 包含 protocol_versions、host/plugin 身份、instance_id、capabilities 和 limits；插件响应所选 protocol_version、plugin 身份及 capabilities。当前宿主只提供版本 1，身份和能力须与 manifest 一致。响应须匹配请求 ID，result/error 二选一；错误版本、无效响应、stdout 日志或未经请求的帧均关闭该实例。日志写 stderr。

初始化最多 30 秒，调用方可提前取消；准备上下文结束不关闭已就绪实例。所有者调用 Close 时先请求 `plugin.shutdown`，最多等 2 秒，再对进程组发送 TERM，最多等 300 ms 后发送 KILL，回收主进程并关闭管道。退出时诊断最多再排空 100 ms；脱离原进程组的外部进程不在此回收保证内。

协议帧上限 8 MiB。stderr 持续读取，保存脱敏后的末尾 64 KiB，记录读取和淘汰字节数，展示时转义控制字符。显式环境引用值，以及继承环境中名称含 KEY/TOKEN/SECRET/PASSWORD/PASSWD/CREDENTIAL/AUTH 的值用于脱敏；单值最多 64 KiB、最多收集 256 项。跨读取边界的凭据仍脱敏，最多暂存最长凭据长度减一的尾部，关闭时冲刷。未知或编码后的敏感值需插件自行处理。

Info 返回实例身份、manifest 摘要、协商版本、状态、PID、能力、限额和 trusted_execution。摘要覆盖启动配置，不是可执行文件哈希。插件按当前用户权限运行；进程分离用于生命周期管理，外部副作用不随关闭回滚。初始化错误返回的实例已关闭，仅供诊断，失败候选不影响其他实例。

### 注册与目录查询

```sh
zende --request-id install-plugin resource register AGENT /path/to/plugin.yaml --kind plugin
zende resource list AGENT --kind plugin
zende resource status AGENT RESOURCE
zende plugin catalog AGENT RESOURCE
zende plugin catalog AGENT RESOURCE --version VERSION
zende resource read AGENT RESOURCE --version VERSION
zende --request-id refresh-plugin resource refresh AGENT RESOURCE
```

注册使用 Agent cwd 解析相对路径。初始化和目录发现共用 30 秒预算，完成后再复读 manifest；校验成功才事务保存 manifest 原文、目录、发布版本和回执。包中只有凭据引用，启动时解析的值留在进程边界。非法定义、源文件变化、取消或过期候选均关闭，更新失败保留旧版本与旧实例。

`catalog` 返回已保存的完整 tools/commands 定义、schema、ref、manifest_version 和 catalog_version；`version` 是包含 manifest 原文及目录的资源版本。查询只读数据库，`--version` 指定历史资源版本，`read` 读取该版本 manifest。目录仍受 256 项/4 MiB 的发现限额约束，读取时重新核对 schema、引用和版本。

`status` 区分进程可用性与最近更新状态。客户端退出后插件由 Core 持有；Core 关闭时回收进程。重启后保留目录，进程状态为 unavailable，显式 refresh 准备新实例。重复已接受请求只返回原回执；同版本 refresh 保留健康实例，离线时重新启动。修改可执行文件后应更新 manifest 的 version，因为摘要不覆盖二进制内容。

当前发布只管理发现实例，尚未开放插件执行入口；新版发布后回收旧发现实例。执行接线将加入调用持有和旧实例退役，8.4 验收前不宣称运行中调用可热替换。

### 插件调用底层

初始化后，`Instance.Discover` 通过 `plugin.describe` 读取已声明的 tools/commands，验证名称、描述和 input_schema/output_schema，再固定本实例的目录。定义引用包含插件身份、manifest 摘要、能力类别和完整定义；JSON 键顺序及新增无关定义不改变原引用。更新定义须准备新实例。

`PrepareCall` 选择引用、校验参数并保存有效 Scope；`Binding/Definition/Scope/Arguments` 返回可记录的绑定。所有者须先持久保存派发意图，再执行一次 `Invoke`，并持有实例直至返回。目前尚未接入 Core 持久回执、模型工具目录和 CLI 命令；跨重启去重也由这些入口负责，底层不重试调用。

| 协议消息 | 内容 |
| --- | --- |
| `plugin.tool.call` / `plugin.command.call` | 请求包含 operation_id、definition_ref、name、scope、arguments；两类输出由各自 adapter 解释 |
| `plugin.progress` | 无 id 的通知；params 含 request_id、operation_id、从 1 连续递增的 sequence 和 JSON value |
| `plugin.cancel` | 宿主发送的无 id 通知，关联 request_id 和 operation_id；仅表示请求取消 |
| 原调用响应 | result 含 operation_id、state、output 和可选 error；终态接受 completed、failed、cancelled |

`Invoke` 按线序交给进度观察者，观察者应保存成功后再返回。取消后最多等 5 秒确认终态；插件仍可返回已完成结果，完整结果优先于随后断线。没有可验证响应、输出 schema 不符或进度保存失败时返回 operation_unknown，并保留收到的有界响应作证据；终态不承诺撤销外部副作用。

每实例最多 8 个请求，写帧最多等 2 秒；进度每条 16 KiB、每请求最多排队 32 条，越界关闭实例并让相关未完成调用待核实。目录最多 256 项/4 MiB，单定义 256 KiB，参数 1 MiB、结果 4 MiB。JSON 深度最多 64，数字最多 1024 字符、指数绝对值最多 1024；schema 支持 draft-07/2020-12，关闭外部文件和网络加载。目录、大小、并发和等待限额随初始化及 Info 返回。调用证据可能含业务数据，不能直接混入普通诊断或模型上下文。

## Responses 接入

连接使用 `protocol: responses`，`base_url` 为 API 根地址，adapter 追加 `/responses`。型号、输出上限和可选思考力度均来自配置。`native_categories: true` 仅用于已确认支持 assistant `phase` 的具体型号，默认使用显式文本标签。其他三种协议的配置元数据已接受，adapter 由后续任务交付；当前选择它们会明确报不支持。

| 内容 | 当前处理 |
| --- | --- |
| 请求 | `stream:true`、`store:false`、`truncation:disabled`；发送完整已提交上下文和 `max_output_tokens`，不依赖服务端历史 |
| 工具 | function 定义使用 `strict:false`，保留可选参数；host 校验参数。按 item/call ID 组装交错流，完整响应校验后执行，结果回填 `function_call_output` |
| 可见输出 | `output_text`、`refusal` 映射为文本；`phase: commentary/final_answer` 映射为 commentary/final。类别晚到时暂存可见正文，既不推断也不提前结束任务 |
| 私有数据 | 保存完整 message/function_call/reasoning item，包括 ID、phase、annotations、summary、encrypted content 和未知附加字段；同端点、同型号续接时按原顺序回传 |
| 错误与取消 | HTTP 状态码分类，响应体和传输 URL 不写入错误。拒绝重定向；取消关闭请求。部分参数、提前 EOF、`[DONE]`、失败或不完整响应均不授权工具执行 |
| 用量 | 保存返回的 input/output token 数；缺失保持未知。细分 token、价格与缓存计量待接入 |

请求最多 32 MiB，SSE 单事件 8 MiB、单次流 64 MiB；公共组装仍受单块 1 MiB、响应 4 MiB/128 块限制，私有数据计入额度。服务端的 `response.completed` 必须与已结束 item 一致。响应结束只通过传输校验，任务完成仍需显式 final。

`reasoning` 只保存为协议数据，不进入可见正文。端点、型号或协议变化导致私有数据不兼容时返回 `protocol_mismatch`，原历史保留。user 图片输入见上一节；assistant/tool 附件、内置远端工具、原生 tool search、自动重试和上下文压缩仍待接入。遇到不支持的输出事件/类型会报错，`limits.model_retries` 此阶段不驱动 HTTP 重试。

协议与本地源码依据：OpenAI [流事件](https://developers.openai.com/api/reference/resources/responses/streaming-events)、[函数调用](https://developers.openai.com/api/docs/guides/function-calling)、[reasoning](https://developers.openai.com/api/docs/guides/reasoning)；Pi `7bc154ad082e2f0029c19117a16d22a504439476` 的 `packages/ai/src/providers/openai-responses{,-shared}.ts`，Codex `7219fd735bef2f9cfd0363fecdbbb212e3df5255` 的 `codex-rs/codex-api/src/sse/responses.rs`。实现及测试不代表任意兼容端点支持这些字段；真实端点、凭据和原生 phase 仍未验收。

# AI-Native Harness Principles

状态：已确认的架构原则，配合 spec v0.1 实施

更新日期：2026-10-16

行为定义与验收条件见 [SPEC.md](./SPEC.md)。本文按最新讨论更新；源码调研报告中的早期架构建议不覆盖这里的决定。

## 开发原则二：绝不测试

按 2026-10-16 的决定，后续开发直接推进实现，不新增、不运行测试，也不以冒烟或验收脚本替代。已有测试代码和历史结果保留；后续改动标注未验证，不宣称测试通过。

这条约束针对本项目的开发流程。harness 的终端执行能力、运行时输入校验和错误处理保持完整。

## 一句话

给文本模型一个可以持续观察、执行、修改和恢复的电脑世界，让它通过终端、进程和事件流工作。工具按能力边界组织，不模拟人类的 CRUD 动作。

## 1. 第一性原理

### 1.1 模型不是人类的缩小版

模型不需要被迫按照“先读文件、再找内容、再编辑文件”的人类工作步骤行动。模型真正需要的是：

- 可以观察当前环境；
- 可以启动和控制进程；
- 可以接收持续输出；
- 可以修改持久状态；
- 可以根据结果继续行动。

工具设计应围绕模型的能力和反馈回路，而不是围绕人类动作的拟人化拆分。

### 1.2 终端是电脑世界的主要载体

终端不是单纯的文本界面。它同时提供：

- 命令执行；
- stdin/stdout/stderr；
- PTY 和 pipe；
- 长进程和交互式程序；
- 编译、测试、调试和脚本生态；
- 可以远程化、重放和投影的事件流。

因此 harness 的核心不是“给模型几个工具”，而是构造一个更适合模型使用的终端和进程运行时。

### 1.3 能力优先于动作拟人化

我们优先暴露模型真正需要的能力边界：

```text
观察环境       → command output / process output
启动执行       → exec
继续执行       → process session / stdin
修改文本       → patch
改变终端尺寸   → resize
停止任务       → cancel / terminate
获取持续反馈   → event stream
```

工具名称和 API 不是目的。稳定的能力边界才是目的。

## 2. 终端与进程原则

### 2.1 PTY 是一等能力

真实开发环境中，很多程序依赖终端语义：交互式 shell、REPL、全屏程序、密码提示、终端尺寸、颜色和控制序列。PTY 不能被普通的“一次性 shell 调用”替代。

day1 必须支持：

```text
spawn
stdin
resize
output
exit
terminate
```

PTY 不是所有命令的默认模式。非交互命令可以使用 pipe，以获得更稳定、更容易解析的输出。正确的原则是：

> PTY 必须存在，pipe 必须保留；由运行时和命令需要决定使用哪一个。

Day1 使用 tmux 承载 PTY，并作为必需依赖。tmux 不替代 Agent 历史、mailbox 或进程输出记录；非交互命令仍可由 runner 通过 pipe 执行。

### 2.2 进程是有身份和生命周期的对象

长任务不能被当成一次函数调用处理。每个活动进程需要有：

- process id；
- cwd 和环境；
- PTY 或 pipe 类型；
- stdin 状态；
- 输出缓冲和截断状态；
- started、running、exited、failed、cancelled 状态；
- exit code 和终止原因。

`write_stdin`、`resize`、`terminate` 都是对已有进程的操作，不是重新启动命令。

### 2.3 终端输出和 UI 渲染分离

PTY 不得直接写 TUI renderer 的 stdout。运行时先产生事件：

```text
pty.started
pty.output
pty.resized
pty.exited
```

TUI、桌面端和 CLI 都从事件投影自己的展示。需要完整接管终端的全屏程序，必须显式执行 terminal handoff，不能把原始 PTY 字节混入普通 UI 渲染。

首版只交付 TUI，不实现桌面端。核心与展示分离仍是约束，不因首版只有一种 UI 而取消。

## 3. 观察与读取原则

### 3.1 读取保持开放

模型可以根据问题选择不同的观察方式：

```bash
cat
sed
rg
find
git diff
go test
自定义脚本
```

不强制把所有读取行为收敛成一个 `ReadFile(path, range)` 工具。不同观察方式携带不同的信息和视角；限制读取方式会降低模型面对真实环境的能力。

### 3.2 开放读取不等于无记录

runtime 仍记录命令、输出、耗时、process id、退出状态和关联的 Agent、task 与请求。我们不统一读取的语义，但统一执行的生命周期和事件 envelope。

### 3.3 不把终端输出误认为模型上下文

原始 stdout/stderr、PTY bytes、结构化工具结果和模型可见上下文是不同层次：

- runtime 可以保留原始输出；
- 输出需要截断和预算控制；
- 只有经过工具结果转换的内容进入下一轮模型上下文；
- TUI/桌面端可以展示比模型上下文更多的内容。

## 4. 写入与变更原则

### 4.1 写入默认采用规范化路径

持久状态变更需要比读取更明确的表示。默认使用 `apply_patch`，因为它可以提供：

- 局部变更，而不是重写整个文件；
- context 和 old lines 校验；
- add/delete/update/move 语义；
- 结构化文件变更事件；
- TUI 和桌面端一致的 diff 展示。

Patch 是 agent-facing 的内部变更协议，不是 Git 依赖，也不是行业通用标准。

### 4.2 规范化写入不是能力限制

`exec` 仍然可以运行格式化器、代码生成器、迁移工具、构建工具和用户脚本。这些程序可能直接修改文件，不能因为展示困难就禁止它们。

因此：

```text
apply_patch = 默认、可观察、可校验的文本修改路径
exec        = 运行真实程序，保留环境的完整执行能力
```

如果需要覆盖命令产生的文件变更，再增加 workspace diff observer，而不是把所有写入强制改造成 patch。

### 4.3 变更不是权限系统

写入规范化解决的是表示、校验、观察和恢复问题，不是权限管理。当前项目 day1 不做 permission manager 和 sandbox；工具与插件属于受信任执行边界。

即使没有权限系统，也必须保留 timeout、cancel、process group 回收、输出上限、exit status 和错误传播，因为这些属于运行时正确性。

## 5. Agent runtime 原则

### 5.1 Agent 是状态机，不是一次请求

一次 task 至少包含：

```text
输入
  → 模型增量输出
  → tool call
  → tool progress/result
  → 下一轮模型输入
  → 完成、失败或取消
```

长进程、PTY 输出、模型增量和用户输入都必须能够在这个状态机中继续发生。

### 5.2 Tool loop 不依赖 UI

Agent runtime 不导入 Bubble Tea、桌面组件或具体 renderer。它只处理：

- model adapter；
- tool registry 和 executor；
- process session；
- command/event；
- Agent 会话、task 与模型请求；
- cancellation 和 persistence。

TUI 和桌面端都是 runtime 的消费者，不是 runtime 的拥有者。

关闭 TUI 只断开连接，Agent、PTY 和 pipe 继续工作；重新打开可以接回。后台 core 的生命周期独立于 TUI。用户停止当前 Agent、停止全部和关闭界面是不同操作。

### 5.3 请求与调用固定版本，能力可以即时加入

任务记录启动配置，每次模型请求和工具调用记录实际采用的资源版本。已经发出的请求、正在输出的 provider 流和已经执行的工具调用保持稳定，不被热更新替换。

新安装的 MCP、skill 或插件在注册并就绪后，当前会话、当前任务即可发现和使用，不等待最终回答，也不要求重启。新增能力通过目录发布、安装结果和追加的变更通知传入模型，不改写旧历史。

默认模型、cwd 等运行选择不会仅因配置刷新而在当前任务中静默切换；显式选择可以改变后续请求，新任务使用最新默认值。可用能力目录不再被整项任务的启动快照锁死。

### 5.4 热加载是资源热加载，不是进程内替换 Go 代码

day1 的热加载包括：

- 配置；
- prompt；
- skills/resources；
- tool/plugin manifest；
- provider registry；
- 外部插件进程的版本切换和重新注册。

先准备并校验新版本，再发布能力。旧调用绑定旧版本并完成后回收，失败的更新不替换旧可用版本。安装和刷新能力本身必须可被模型调用。

不承诺在运行中的 Go 进程内替换任意代码。热加载完成后发布：

```text
resource.reloaded {
  generation
  hash
  changed_resources
}
```

## 6. 输出、事件与多端原则

### 6.1 核心输出只有一份语义事实

模型输出、工具生命周期、PTY 输出和 task 状态先进入统一事件流，再分别投影到 TUI、桌面端和 CLI。

```text
runtime event stream
        ├── TUI projection
        ├── desktop projection
        └── CLI/JSON projection
```

“输出一样”指事件顺序、内容块、工具生命周期和关联 id 一致，不要求不同端拥有相同的换行、颜色、宽度或像素布局。

### 6.2 语义事件和原始字节分开

普通模型文本使用 `ContentBlock` 和语义 delta。PTY 输出保留 raw bytes/encoding 信息，由终端投影层决定是否运行 terminal emulator。不能把 ANSI 控制序列当成通用模型文本。

### 6.3 事件 envelope 必须可关联和重放

事件至少包含：

```text
schema_version
agent_id
seq
kind
payload
```

`agent_id` 是持续会话身份；`seq` 在该会话内单调递增。涉及任务时带 `task_id`，请求、工具、消息和进程的身份按需关联。事件可以实时订阅，重连后按游标补发；持久化介质属于 spec 中的工程选择。高频 PTY chunk 不要求全部进入模型历史，但必须有输出读取位置和明确的截断、丢失策略。

### 6.4 模型显式区分过程说明和最终回答

Day1 支持模型明确输出 `commentary` 和 `final`。它们是可见的过程说明与最终回答，不是隐藏推理。协议有可靠的原生语义时直接使用，否则通过提示词约定的显式标记适配。

不能根据 finish reason、是否还有工具调用或文字语气猜测 final。消息种类也不意味着所有事件都要增加通用 `phase` 字段；具体解析和失败行为由 spec 规定。

## 7. 协议原则

### 7.1 协议分层，不做万能协议

三类协议分别处理：

```text
Model protocol  → Anthropic/OpenAI/其他模型协议
Tool protocol   → MCP/LSP/native tool/PTY tool
Host protocol   → TUI/CLI/Desktop/JSON-RPC/WebSocket
```

内部只统一实际需要的消息、调用和事件语义，保留 provider 特有的信息。不强行拼成一个巨型 schema，也不以“必须有三种 IR”为目标。

### 7.2 核心 runtime 不依赖传输层

Day1 的 TUI 和 CLI 都连接独立存活的后台 core。未来前端复用同一组命令与事件；传输层负责连接和编码，不能改变 runtime 的事件语义。

day1 不要求同时实现所有网络传输，但必须让新增传输只增加 adapter，不改 agent loop。

### 7.3 SSE 只能作为事件传输

SSE 适合单向事件订阅，不适合作为唯一控制协议。prompt、steer、PTY stdin、resize、cancel 都需要双向 command 通道。

## 8. Day1 边界

### 必须具备

- Go、Bubble Tea v2 和 alt screen；
- TUI 与后台 core 分离，关闭后工作继续，重连可接回；
- tmux 作为 PTY 后端和必需依赖；
- PTY 和 pipe 双路径；
- process session 生命周期；
- stdin、resize、terminate、cancel；
- `exec`、`write_stdin`、`apply_patch`；
- CLI-first 多 Agent 与常驻的原生协作说明；
- 持久 mailbox，默认 queue，显式 steer 加急；
- 默认共享工作目录，可选 worktree；默认继承上下文，模型可调长度；
- 原始历史 append-only、自动/主动压缩和故障恢复；
- 模型显式 commentary/final，核心输出与展示解耦；
- Responses、Chat Completions、Anthropic Messages、Gemini 四协议；
- MCP 默认搜索后调用，MCP/skill 安装就绪后在当前任务直接可用；
- 请求和调用的资源版本记录，以及资源热加载；
- 独立进程插件提供工具、provider、CLI 命令、skills/prompt 资源和事件订阅。

### 明确不做

- permission manager；
- sandbox；
- approval flow；
- 桌面端 UI；
- 把所有读取行为封装成固定 `Read` 工具；
- 为了模仿 Claude Code 而一次性复制完整语义工具表；
- 把 TUI Model 当成业务核心；
- 插件 marketplace、任意 Go 代码的进程内热替换；
- 四协议共同交付线之外的全部供应商特有功能；
- 为远期评测预建复杂 checkpoint 框架。

## 9. 模块边界

```text
runtime/       Agent、task、mailbox、取消、model-tool loop、event seq
model/         provider adapter、公共消息语义、stream
tool/          tool descriptor、registry、executor、result/progress
process/       runner、tmux PTY、pipe、stdin、resize、terminate、exit
resource/      config、prompt、skill/plugin/tool/provider 版本与热加载
protocol/      CLI、TUI 的 command/event adapter
store/         原始历史、消息投递、资源版本、压缩记录、recovery
render/        Bubble Tea alt-screen
```

职责与数据流：

```text
model/tool/process/resource
          ↓
       runtime
          ↓
   command/event stream
      ↙          ↘
    TUI          CLI
```

`runtime` 不导入 UI。`process` 不直接写 UI。`render` 不直接调用 provider、shell 或数据库。

## 10. 架构决策检查表

以后增加一个工具、协议或服务时，先问：

1. 它是否让模型更直接地操作真实环境？
2. 它是否保留了读取和执行的开放性？
3. 它是否让持久状态变更更容易校验和展示？
4. 它是否保留了长进程的身份和生命周期？
5. 它是否通过事件而不是 UI 私有状态表达结果？
6. 它是否可以在 TUI 和桌面端之间复用？
7. 它是否保证已有请求/调用稳定，同时允许新能力在当前任务中生效？
8. 它是否引入了不必要的权限、沙箱或拟人化抽象？
9. 它失败时，模型是否能获得足够具体的错误和后续行动信息？

如果一个设计主要是为了让实现看起来更像人类工具，而没有增加模型能力、反馈质量或状态可恢复性，应当延后或拒绝。

## 11. 参考依据

这些原则来自本次源码调研中的共同结构：

- Codex：`exec_command`、`write_stdin`、Unified Exec process manager、PTY/pipe、`apply_patch` interception；
- Pi：agent loop、AgentSession、RPC/interactive/print 多入口复用；
- Crush：Go wiring、Coordinator、typed pubsub、TUI/runtime 边界；
- Bubble Tea：`Msg`、`Cmd`、`View` 和 renderer 分离；
- zenfun-code：流式 tool loop、tool orchestration、MCP 动态注册。
- OpenCode：MCP catalog、token-budget tool exposure、CodeMode 和 `$codemode.search`。

详细证据见 [`AI-HARNESS-SOURCE-REPORT.md`](./AI-HARNESS-SOURCE-REPORT.md)。

## 12. Tool search 与 MCP 的第一性原则

MCP 是能力目录和调用协议，不是模型上下文策略。`tools/list` 负责发现，`tools/call` 负责执行；是否展开、如何搜索、何时把定义交给模型，由 harness 的 host 和 provider adapter 决定。

day1 默认不把 MCP 工具平铺进模型上下文。外部工具保持 deferred，host 建立带 `generation` 的 catalog 和索引；模型通过两个稳定入口完成发现和调用：

```text
tool_search(query, source?, namespace?, limit?, cursor?)
  -> tool_ref + description + input_schema + catalog_generation

tool_invoke(tool_ref, arguments)
  -> structured content / attachments / error
```

`tool_ref` 绑定工具及其定义版本，`catalog_generation` 标识发现时的目录。目标定义被替换或删除、且旧版本不可用时，返回可理解的 stale-reference 错误；不能静默把旧引用映射到新工具。仅新增无关能力不应使所有引用失效。搜索结果是发现，不是权限检查；当前项目不因此引入 permission manager 或 sandbox。

Provider adapter 按经过确认的实际能力选路：支持原生 deferred/tool search 的具体接口走 native mode，否则走 host 的搜索/调用适配。四协议都要有可用路径，不能按品牌或“OpenAI compatible”标签推断原生能力。

OpenCode 的 CodeMode 说明了更强的后续方向：模型只看到一个受限 `execute`，在命名空间内搜索并程序化串联工具。但它包含解释器、预算、schema 边界和 nested metadata，属于第二阶段；day1 先保持 `tool_search + tool_invoke` 的可观测、可测试和跨协议稳定性。

## 13. Agent 与 skill 的状态所有权

CLI 和工具是模型到 runtime/resource service 的入口，不是 Agent 或 skill 的生命周期所有者。

```text
Agent                    = 可恢复的持续会话
Agent CLI                = spawn / send / inspect / wait / stop / resume
SkillPackage             = manifest + instructions + resources
Skill 资源入口           = discover / read / reload
PTY tools                = 真实执行脚本和程序的底座
```

主 Agent 和子 Agent 使用同一种持续会话模型，父子关系记录委派来源。创建命令返回身份和接收状态，完成结果通过 mailbox 回到委派方；wait 可以等待指定任务，但不拥有或取消被等待的任务。Agent 完成一次任务后仍可接收后续工作。

默认共享工作目录，需要隔离时可选 worktree；默认继承父 Agent 当前全部可继承上下文，模型可调整长度或自行提供背景。上下文继承不等于文件隔离，也不意味着后续历史自动同步。

mailbox 持久保存通信，queue 等当前任务结束，steer 在下一安全边界进入上下文。关闭界面不停止工作；停止当前 Agent 不默认停止子 Agent。用户已暂停的 Agent 不因普通结果回报而自动恢复。

skill 应由 catalog、generation、正文和资源组成。显式选择后可以注入 prompt；远端或 executor 资源通过 `list/read` 延迟读取；脚本仍通过 PTY/process 执行。不要设计一个包揽发现、加载、执行、回收的万能 `skill.run`。

## 14. append-only、压缩与恢复

原始历史只追加，正常请求增量构建。模型窗口接近阈值时自动压缩，也允许用户或模型主动发起。压缩新增摘要与原文引用，再切换后续请求使用的上下文视图；原文仍可查询，不静默删除或覆盖。

压缩不能拆散工具调用与结果，不能遗漏压缩期间到来的消息。压缩失败保留原状态；无法继续时给出可恢复错误，而不是清空会话。

harness 故障后恢复已有历史、任务、mailbox 与可关联的进程，不让用户重新交代整个任务。自动恢复只继续可确认的工作；外部操作可能已执行但结果未知时先核实，不盲目重跑。退出状态、消息去重和资源版本必须可追溯。

## 15. Interface everywhere 与能力说明

模型不仅能执行功能，也能查询 Agent、进程、资源和历史，订阅事件并接收有意义的变化通知。暴露接口是为了信息可达和主动工作，不是把每个内部字段变成工具。

Agent 协作以 CLI 为主要入口。原生提示词提前给出使用时机、核心命令、常用参数、默认值和结果回流规则，让普通委派无需先查几轮帮助。低频细节通过 `--help` 展开。

核心协作能力提前告知；数量多、变化频繁的 MCP 能力搜索后使用。这两种发现策略可以并存。扩展安装、刷新与就绪查询也必须对模型开放，形成“发现缺少能力 → 安装 → 就绪 → 当前任务使用”的闭环。

## 16. 所有可点击入口都有对应的 `/命令`

用户输入 `/` 就能搜索、选择并直接进入界面功能。模型、工作目录、Git、后台任务、会话、警告详情等可点击入口，都必须提供对应的 `/命令`。

点击与 `/命令` 调用同一功能，使用相同的参数、状态与确认流程。鼠标和快捷键是补充入口；仅支持 Tab 移动焦点、Enter 激活，不能替代 `/命令` 直达。

命令提示按需展开，保持主界面干净。这是后续新增界面入口也必须遵守的原则。

## 17. 输入优先，按需展示

主界面保留对话、组合输入框和下方状态栏。状态栏展示模型、cwd、Git；后台工作和警告有内容时才出现。减少可点击按钮，提示只在输入、聚焦或展开时出现，发送方式不常驻展示。

`/` 命令与 `$` skill 的候选从输入框下方展开。主对话和模型提问复用输入组件，支持多行、中文输入和图片附件。commentary 可折叠；错误首次展开，后续从状态入口查看。

沿用自定义 Tango Dark 配色，正文与次要文字使用绿色层次。字体优先 JetBrains Mono Nerd Font；真实终端由用户选择字体，缺字时提供普通符号。

## 18. 提示词来自可用能力

参考 Pi 的短系统提示词、工具短说明与使用规则分离、按可用能力组装的方式。系统说明职责和必要行为，工具说明输入、结果、限制和继续操作的方法；协作常用命令提前说明，低频参数通过帮助查询。

提示词随实际注册能力生成或校验。skill 先发现再读正文，MCP 先搜索再调用。更新追加到上下文，保留来源和版本。精简重复措辞，保留默认值、失败条件和恢复办法。

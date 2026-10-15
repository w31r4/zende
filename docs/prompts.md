# 提示词

系统说明由 `internal/runtime/prompt.go` 组装，工具 descriptor 由各 executor 提供，MCP 说明位于 `internal/runtime/mcp_tools.go`；请求前保存为 `prompt.loaded`。CLI 说明覆盖提问、资源、工作环境、选配和 Agent 协作；资源更新在后续请求追加发现通知。MCP 已接入搜索、调用和结果续读，外部目录监视待完成。

## Pi 参考

参考本地 `.research/pi-agent`，提交 `7bc154ad082e2f0029c19117a16d22a504439476`：

- `packages/coding-agent/src/core/system-prompt.ts`：只列可用工具，组合短说明、规则和项目上下文，去重共同规则。
- `packages/coding-agent/src/core/tools/bash.ts`：说明工作目录、结果、输出上限和继续读取办法。
- `packages/agent/src/harness/system-prompt.ts`：skill 先列名称、描述和位置，匹配后读正文，资源路径相对 skill 目录。
- `packages/agent/src/agent-loop.ts`：模型响应、完整调用、结果回填、下一次请求的顺序；Zende 将这些内容保存为追加事件，并独立判断显式 final。

Zende 保持 exec、进程继续操作和 patch 的工具边界。Pi 的 read/edit/write 拆分及强制使用 Read 的规则不移植。

## 组织

系统提示词列职责、已注册能力和输出类别；工具 descriptor 保存 description、snippet、guidelines 和版本。协作命令注册后才加入，长参数留给 schema 与 `--help`。

项目指令和 skill 保留来源、版本。组装结果在请求发出前保存；后续变化追加新记录，不修改先前 prompt。凭据和 provider 不透明续接字段不进入公共说明。

文案按 trim-cn-tech-doc 精简：删除重复规则和自我解释，保留参数、默认值、状态区别、错误处理及恢复步骤。描述“返回 process_id 后如何继续”，不把 accepted 写成 completed，也不把空输出写成进程退出。

已注册的进程工具为 `exec_command` 和 `write_stdin`。说明直接给出工作目录、pipe/PTY 选择、等待上限、输出游标和原 process ID 的继续方式。完整参数在 JSON schema 中；读取自由走命令，输出保留字节游标，正文转义终端控制字符。参数与结果见 [进程工具](./processes.md#模型工具)。

`apply_patch` 的 descriptor 给出最小语法示例、精确上下文、目的地冲突和部分应用后的处理办法；系统提示词只保留默认编辑规则。shell 生成器仍可写文件，说明和实际行为由模型循环测试共同验证。语法、限额和来源见 [文本 patch](./patch.md)。

进程工具版本 2 增加停止观察后的续接说明：保留 process ID，`observation_interrupted` 表示最后观察状态。Task 取消完成时追加 developer 提醒，说明已执行效果保留、后台进程可能继续；resume 新建任务，不重新派发已完成调用。

核心提示词版本 2 增加 Task ID。CLI 启动的 core 提供 `question ask/inspect` 短说明，只有注册了 `exec_command` 才展示；命令使用当前二进制绝对路径和 profile，路径作 shell 引用。说明保留提问时机、8 KiB/8 项限额、回答进入上下文的时点、重试身份及不得代用户回答。恢复旧 Task 继续使用已保存的提示词。该组织方式沿用 Pi 的按能力组合规则，问题由 core 持久保存。

版本 3 增加 `resource list/read/register` 说明，沿用同一 exec 能力条件和路径引用。参考 Pi `packages/coding-agent/src/core/skills.ts`，先查询名称、描述和来源，再按返回版本完整读取正文；禁用模型自行调用的 skill 需用户明确选择。安装完成后 register，修改后 refresh，失败保留旧版；脚本通过进程执行。说明只列常用入口，完整参数留给 `--help`。

版本 4 增加 `workspace directory/git` 和 `model connections` 查询入口，仍按 exec 能力组合短说明。目录查询只浏览；Git 读本地状态；型号与力度通过 `model list CONNECTION` 查询。adapter 支持标记与端点/凭据健康状态分开，长参数留给 `--help`。旧 Task 恢复时沿用原提示词。

版本 5 增加 `agent selection/select`，说明下一请求生效、默认力度和原进程 cwd。基础提示词允许后续 developer 选配通知更新 cwd 与输出格式；通知复用同一份分类规则，路径和型号用 JSON 字符串引用。通知与实际请求快照同事务追加，旧请求和提示词保留。结构继续参考 Pi 的按能力组合短说明，完整参数由 CLI 帮助提供。

版本 6 增加 `agent spawn/submit/inspect`，说明委派时机、默认继承、未完成调用排除、立即返回身份和重试方式。继续采用 Pi 的按能力组合短说明；exec 未注册则不展示，长参数留给帮助。send、wait 和自动回流实现后再加入。子上下文追加独立身份说明，继承历史不成为待执行的父任务；旧 Task 恢复继续用原提示词。

版本 7 补充 `last:N`、继承预算和背景入口：默认读当前视图，仅设预算时选最近完整轮次，显式范围超预算报错。提示模型检查回执范围与估算，详细限额留给帮助及 [继承说明](./collaboration.md#继承)。继续按 Pi 的能力条件组合，旧 Task 的已存提示词不改。

版本 8 补充显式 worktree 参数、提交检出与保留目录规则，以及未知创建使用原请求 ID 核实的入口。仍放在已注册 spawn 的短说明中，完整限制见 [Worktree](./collaboration.md#worktree)；旧 Task 的提示词不改。

版本 9 加入 `agent send` 和 `mailbox list/inspect`。短说明给出 queue/steer 的时点、暂停保护、三种消息状态及原 ID 重试；参数与额度留给帮助。沿用 Pi 的能力条件组装，旧任务继续用已保存的提示词；自动回流和 wait 实现后再加入。

版本 10 加入 `agent wait/result`。说明只在依赖子结果时等待、指定 Task、超时不取消、自动 queue、完整结果送达后去重，以及分页读取。纯结果通知无需自动回执。继续按 Pi 的已注册能力组合，详细预算与游标留给帮助；恢复旧 Task 保留原提示词。

版本 11 加入 `agent stop/resume`，区分默认单 Agent 暂停和显式 `--tree`，说明完成查询、未知效果、队列保留和原 ID 重试。沿用 Pi 的能力条件短说明，完整范围和限额见帮助及 [停止与恢复](./collaboration.md#停止与恢复)。一致性测试核对命令注册、默认 `tree=false` 和不可用状态；旧 Task 仍用保存的提示词。

版本 12 更新资源 CLI 短说明：`status` 区分已有版本可用和最近更新失败。沿用 Pi 的按能力组合方式，参数和状态细节留给帮助及资源文档；旧 Task 保留原提示词。

版本 13 增加 `mcp.search` 和 `resource register --kind mcp`。先发现服务，再搜索完整工具定义及版本引用；不平铺目录，服务端描述仍是数据。MCP 发现通知要求搜索入口已注册。继续参考 Pi 的短说明与能力条件组装，旧 Task 的基础提示词不变；工具执行未实现，不提前提供调用说明。

版本 14 加入原生 `tool_search`、`tool_invoke` 和 CLI `mcp.result` 说明。descriptor 给出搜索引用、参数校验、结果分页及未知效果不得重发的规则；服务端描述和结果始终是数据。系统说明继续按 Pi 的能力条件组合短句，详细参数留给 schema 和帮助。完整 MCP 定义按需进入工具结果，旧 Task 的已存提示词不改。

## Skill/prompt 的请求级更新

`resource.register/refresh` 校验并保存不可变文件包；失败保留上一版。任务每次请求从同一数据库事务读取上下文和当前资源目录，`model.started.resource_catalog` 保存 generation 及全部 ID/版本。它记录当时可发现的定义，不代表正文已经应用。

`internal/runtime/resource_notice.go` 在目录变化后的下一请求追加 developer 通知，来源为 `zende/resources`、格式版本 1。只在 exec 和资源 list/read 入口已注册时展示，最多 8 项、12 KiB；超出部分通过 CLI 搜索和分页查询。同一资源的多次更新合并为当前版本，禁用模型自行调用的 skill 留给用户选择，正文和脚本按需读取。

通知与 `model.started` 同事务提交。组装期间目录变化则重建请求，正在输出的请求保持原版本；默认模型、cwd 不随发布改变。恢复读取原请求的 generation，目录未变时不重复通知；上下文视图切换后重新提供当前发现信息。原 prompt、通知、读取结果和已发布正文保留。

```sh
zende resource register AGENT /absolute/path/to/skill
zende resource list AGENT --kind skill --query review --limit 20
zende resource status AGENT RESOURCE
zende resource read AGENT RESOURCE --version VERSION
zende resource read AGENT RESOURCE --version VERSION --path scripts/run.sh
zende resource refresh AGENT RESOURCE
zende resource register AGENT /absolute/path/to/prompt.md --kind prompt
```

skill 的 `SKILL.md` 与 prompt 文件使用 YAML frontmatter，要求 `name`、`description` 和非空正文。skill 包含相对路径资源，文本按 UTF-8 返回，二进制按 base64 返回。版本覆盖文件路径、字节和执行位；原安装文件更新或删除后，已发布版本仍可读。脚本通过进程工具执行，读取时不运行；执行固定版本脚本时使用该版本的字节，不把可修改的安装目录当作版本副本。

通知沿用 Pi 的能力条件和“元数据发现 → 完整读取 → 使用”顺序，使用 Zende 的版本化 CLI。`TestModelInstallsRefreshesAndUsesSkillThroughBinaryCLI` 用真实 CLI、UDS 和 shell 验证同一 Task 安装、刷新、读取 skill/prompt、执行脚本；模型使用确定性夹具。

## 输出分类

只有 adapter 声明支持可靠原生类别时，才直接接受 commentary/final。其余接入要求以下独立行标记；控制行不带缩进或多余空格，正文保留 LF/CRLF：

```text
<commentary>
可见的过程说明。
</commentary>
<final>
最终回答。
</final>
```

允许多个 commentary，最多一个 final，块不嵌套，final 后仅允许空白。引用、反斜杠转义和代码围栏里的同名文本作为正文；用户和工具消息不进入分类器。标记可跨片段，正文可跨 UTF-8 字节。分类只处理公开回答，不要求输出隐藏推理。

原始 assistant 消息、标签和协议私有字段继续用于合法续接。`model.output.delta` 是带原消息 ID、块序号和类别的临时显示结果；`model.completed.output` 在整个响应合法后确认。标签闭合、消息结束或 finish reason 都不能单独完成任务。

格式错误记录为 `model.output.invalid`，原文保留。完整响应中的工具调用先执行并保存结果，final/tool 冲突随后修复；断流响应中的调用全部不执行。结果 unknown 时先核实原操作，不进入格式修复。

`limits.format_repairs` 默认每个 Task 最多 1 次，可在配置中调整，0 禁用自动修复。`model.started` 记录当前次数、上限和是否为修复请求；`model.repair` 与一次性 developer 提醒同事务追加。修复请求不提供工具定义，即使模型换调用 ID 也只收到拒绝执行的结果。有效 final 完成任务；有效 commentary 恢复正常循环，需要继续工作的工具在后续请求中重新开放。超限保留历史并报告 `output_format`。

当前以本地模型夹具验证两条分类路径。各供应商原生类别字段、真实端点及跨协议续接仍需各 adapter 单独验证。

# 提示词

实现入口为 `internal/runtime/prompt.go`，工具说明契约为 `internal/tool/tool.go`。组装结果在请求前保存为 `prompt.loaded`。已接入提问、资源、工作环境查询、选配和 Agent 创建/复用 CLI；其余协作命令、项目资源自动发现及 skill 目录的请求级更新仍归第 6、7 组。

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

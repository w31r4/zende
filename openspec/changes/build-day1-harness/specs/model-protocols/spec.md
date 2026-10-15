# Spec Delta

## Purpose

让同一 Agent 行为能通过四类模型协议完成，而不把供应商的传输细节交给业务循环。模型明确区分可见过程说明与最终回答，工具结果、上下文预算、错误和取消保持一致且可验证的意义。

## ADDED Requirements

### Requirement: 四协议具备共同执行闭环

系统 SHALL 在 Day1 支持 OpenAI Responses、OpenAI-compatible Chat Completions、Anthropic Messages 和 Google Gemini。每个接入 SHALL 支持文本流、完整工具参数组装、多轮工具调用、工具错误、取消、上下文预算及压缩、显式输出分类、MCP 搜索调用和可查询错误。

#### Scenario: 四种协议运行同一工具任务

- **WHEN** 分别配置四类接入，执行包含至少两轮工具调用和一次 MCP 搜索调用的任务
- **THEN** 每类接入均完成输入、工具结果反馈和显式 final 的闭环，错误能够归因到具体请求或调用

### Requirement: 部分参数不触发工具执行

系统 SHALL 只在工具调用身份和完整参数组装、校验完成后执行，保留每次调用与结果的对应关系。模型流中断或不完整参数 MUST NOT 被当成完整调用；取消 SHALL 保留已记录内容与真实执行状态。

#### Scenario: 参数跨流片段到达

- **WHEN** 工具参数 JSON 被拆成多个片段，或在参数中途断流
- **THEN** 完整且合法的参数才触发一次执行；不完整调用记录为中断，不猜测缺失参数

### Requirement: 供应商专用数据留在所属协议中

系统 SHALL 保留合法续接需要的原始消息与不透明字段，并将共享业务语义与供应商数据分开。切换 provider 时 MUST NOT 把旧协议的不透明数据当作普通文本，也 MUST NOT 静默删除无法转移的用户内容。

#### Scenario: 切换协议时存在专用续接数据

- **WHEN** 用户或模型选择另一协议而当前历史含供应商专用状态
- **THEN** 系统构造可支持的合法请求或明确指出不能迁移的能力；原历史保留，不伪造跨协议续接

### Requirement: 输出类别由模型显式给出

系统 SHALL 将可见模型消息区分为 `commentary` 和 `final`，只接受可靠原生类别或模型输出的约定标记。系统 MUST NOT 根据 finish reason、语气或是否有工具推断 final；分类 MUST NOT 要求公开隐藏推理，也不扩展为所有事件的通用 phase。

#### Scenario: 原生类别或显式文本标记

- **WHEN** 模型以原生类别，或完整的 commentary/final 标记块输出
- **THEN** 各客户端得到相同的消息类别与正文，传输和渲染方式不影响分类

#### Scenario: 普通结束但未给出分类

- **WHEN** 响应正常结束但没有合法类别或标记
- **THEN** 系统保留未分类原文并报告格式问题，不自行当作 final

### Requirement: 显式标记有流式与字面量边界

采用文本适配的接入 SHALL 识别各占一行的 `<commentary>`、`</commentary>`、`<final>`、`</final>`；块不嵌套，允许多个 commentary，最多一个 final 且其后无其他可见回答。解析 SHALL 支持跨片段标记，并忽略用户、工具、引用行、围栏代码或转义中的同名文本。

#### Scenario: 标记分片且正文包含伪标记

- **WHEN** 控制标记跨流片段，正文的引用、代码块或转义行也含 `<final>`
- **THEN** 只有模型正文中的有效独立标记起控制作用，字面示例保持原文，不提前结束任务

### Requirement: 无效输出保留且修复有界

系统 SHALL 对缺失结束标记、嵌套、块外文本、重复 final 和 final 后新增回答报告格式错误并保留原文。修复 SHALL 有可查询上限；修复 MUST NOT 重复已执行工具，失败时 MUST NOT 无限调用模型或假报完成。

#### Scenario: final 中途断流或重复出现

- **WHEN** final 未闭合便断流，或响应出现第二个 final
- **THEN** 输出保持不完整或无效，任务不完成，有限修复失败后报告明确错误

### Requirement: 任务完成等待明确 final 和必要调用终态

系统 SHALL 仅在 final 完整有效且该任务必须等待的调用全部落定后报告完成。同一响应同时要求新工具调用并宣称 final 时 SHALL 先处理状态冲突；后台委派可以继续，其后续结果形成新处理任务，不改写旧 final。

#### Scenario: final 与工具调用冲突

- **WHEN** 模型响应含 final 并要求新工具调用
- **THEN** 系统不提前完成任务，保留调用记录并明确处理冲突，已执行副作用不因格式修复重跑

### Requirement: 可选能力按具体接入验证

系统 SHALL 按具体端点、模型和配置声明可选能力；原生 deferred/tool search 只有在支持被确认时启用，否则使用 host 路径。缺少用量 SHALL 报未知，估算 SHALL 与供应商计量分开，HTTP 成功 MUST NOT 单独作为能力验证。

#### Scenario: 兼容端点缺少可选字段

- **WHEN** OpenAI-compatible 端点不支持某可选字段或没有返回用量
- **THEN** 系统使用已定义的适配或明确报不支持，MCP 搜索调用仍有 host 闭环，用量不伪装为精确零值

# Proposal

## Why

Zende 为文本模型提供可持续观察、执行、协作和恢复的电脑环境。本 change 按已确认的原则与规格交付 Day1。

2026-10-16 执行约定：直接推进实现，不新增、不运行测试或验收脚本。此约定优先于本文及其余规划文件中的测试步骤；保留功能范围、历史测试与结果，新改动标注未验证。

## What Changes

- 建立独立于 TUI 的 Go 后台 core，管理持续 Agent 会话、任务、消息、事件与故障恢复；关闭界面只断开连接。
- 以终端为主要工作入口：tmux PTY 与 pipe 并存，保留进程身份、持续输出和控制；文本修改默认 patch，读取及命令执行保持开放。
- 提供 CLI-first 多 Agent 协作、可选上下文继承与 worktree、持久 mailbox、queue/steer 和明确的停止语义。
- 原始历史 append-only，自动或主动压缩只改变后续上下文视图；故障保留已有工作，未知副作用先核实。
- Day1 交付 Responses、Chat Completions、Anthropic Messages、Gemini 四协议；模型显式区分 commentary/final，核心输出与展示解耦。
- MCP 默认搜索后调用。配置、MCP、skill、prompt 和独立进程插件支持热加载，新能力就绪后在同一任务可用，已开始的请求和调用固定版本。
- 交付 Bubble Tea v2 alt-screen TUI：绿色主题、组合输入、底部状态栏、按需提示与 `/命令` 直达；模型来自实际配置，目录单页选择，会话复制后停止原会话及其后台工作。
- 系统提示词与工具说明参考 Pi 的能力驱动组装，精简重复措辞；完成本机安装、隔离配置档验证与恢复说明。

用户已批准实施。完成情况以任务勾选和验证记录为准；保留根目录原则、规格与历史调研，`.research/` 只读。范围不含权限管理、审批、沙箱、桌面 UI、远程协作平台、插件市场、CodeMode 解释器和评测 checkpoint 框架。

## Capabilities

### New Capabilities

- `durable-agent-runtime`: 独立后台 core、持续会话与任务、停止与恢复、请求去重及可重放事件。
- `terminal-execution`: tmux PTY、pipe、进程控制、输出保存、开放读取与默认 patch 写入。
- `agent-collaboration`: CLI 协作、上下文继承、工作区选择、持久 mailbox 和结果回流。
- `append-only-context`: 历史追加、上下文构建、压缩及原文查询。
- `model-protocols`: 四协议共同能力、最小公共消息语义与显式 commentary/final。
- `live-resources`: 配置来源与生效边界、MCP 搜索调用、skill/prompt 与资源即时加载。
- `extension-plugins`: 独立进程插件、各类能力注册、版本替换与故障处理。
- `tui-client`: alt-screen 交互、重连与事件投影、进程输出展示和尺寸处理。

### Modified Capabilities

无。当前 `openspec/specs/` 没有既有能力规格。

## Impact

- 实施起点为设计文档、浏览器原型、OpenSpec 与参考仓库，本项目 Go 模块从零建立。
- 后续新增的模块覆盖 runtime、model、tool、process、resource、protocol、store 和 render；命令名、IPC、存储布局等工程默认值在 [design.md](./design.md) 中单列。
- 运行依赖包括 Linux、tmux、模型服务以及启用的 MCP/插件；Go 模块与具体版本在实现时选定并锁定，不在规划中伪造验证结果。
- 行为依据为 [PRINCIPLES.md](../../../PRINCIPLES.md) 和 [SPEC.md](../../../SPEC.md)。[源码报告](../../../AI-HARNESS-SOURCE-REPORT.md) 只作历史参考，不覆盖后续已确认决定。
- 完成实现与验证后再同步、归档；原型或夹具通过不代表真实部署验收通过。

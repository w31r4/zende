# Zende

面向文本模型的终端优先 harness，使用 Go 和 Bubble Tea v2。Core 管理持久会话、模型循环和事件，独立 runner 管理 pipe 与 tmux PTY，TUI 可以关闭后接回。

默认文本修改走 patch，读取和程序执行保持开放。MCP 默认搜索后调用；配置、skill、prompt 和独立进程插件可热加载。四种模型协议已有适配实现，真实端点兼容性尚未验收。

当前处于 Day1 实施阶段。已安装用户级命令并启动本机空配置服务，尚未接入用户模型；完整功能与终端交互未验收。详见[交付状态](docs/day1-delivery.md)。

## 安装与启动

运行环境：Linux/WSL、Go 1.26、Bash、GNU coreutils、util-linux；PTY 需要 tmux。

```sh
bash scripts/install.sh
```

脚本构建当前源码，安装到 `~/.local/bin/zende` 并保留旧版；不运行测试或重启已有服务。该目录需在 PATH 中，安装位置可通过参数修改。

按[四协议配置示例](examples/config/four-protocols.yaml)在 `~/.config/zende/default/config.yaml` 中只填写自己的连接和型号。端点示例是占位值；凭据使用 `env:NAME` 或 `file:/absolute/path` 引用，不把 key 写入普通配置或聊天。

配置准备好后，从项目目录启动：

```sh
zende core start
zende runner start
zende --request-id first-session agent create
```

创建结果返回 `agent_id`，用它代替下方的 `AGENT`：

```sh
zende tui AGENT
```

输入框支持 `/` 命令和 `$` skill 候选。用 `/detach` 关闭界面；再次运行同一命令接回，后台工作不因关闭 TUI 而结束。模型只列有效配置中的连接、型号和思考力度。

已有 Core 时，用 `zende config refresh` 发布文件配置；环境变量须在 Core 启动前提供，在其他终端 export 不会改变已有 Core 的环境。配置档使用 `--profile NAME` 隔离。

## 更新与数据

重新执行安装脚本只切换二进制入口。`--list` 列出保留版本，`--rollback RELEASE` 切回旧入口；已经运行的 Core/runner 仍使用原二进制。

数据库、历史和日志默认保存在 `~/.local/state/zende/default/`。旧 schema 迁移前会保留 SQLite 副本，但二进制回退不等于数据库降级或整个运行环境恢复。启停、日志和恢复步骤见[本地开发与运行说明](docs/development.md)。

## 文档

- [原则](PRINCIPLES.md)、[规格](SPEC.md)、[OpenSpec 任务](openspec/changes/build-day1-harness/tasks.md)
- [Day1 交付状态](docs/day1-delivery.md)：部署事实、89 个场景的源码入口和未验收范围
- [运行与配置](docs/development.md)、[进程](docs/processes.md)、[协作](docs/collaboration.md)
- [提示词来源](docs/prompts.md)、[patch](docs/patch.md)、[插件协议](docs/plugin-authoring.md)
- [历史验证记录](docs/verification.md)：按当时版本记录，不代表当前版本整体验收

项目自 2026-10-16 起遵守“不新增、不运行测试”的实施约定；构建和实际部署与测试分开记录。插件和模型命令按当前用户权限执行，不提供审批或沙箱隔离。

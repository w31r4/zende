# Spec Delta

## Purpose

让模型通过非交互 CLI 主动委派、联系和等待同等能力的 Agent，跨多次任务保持通信。上下文与工作目录由明确默认值和模型选择决定，消息持久投递，等待结果不依赖父任务先结束。

## ADDED Requirements

### Requirement: CLI 提供一等 Agent 操作

系统 SHALL 通过 CLI 提供创建、发送、查询、历史、等待、停止、恢复、mailbox 和事件订阅，并与人类客户端共用服务。长文本 SHALL 支持文件或 stdin，机器结果走 stdout、诊断走 stderr；协作 MUST NOT 强制交互式问答或为每个 Agent 打开 TUI。

#### Scenario: 模型主动委派任务

- **WHEN** 模型通过命令提交子 Agent 任务
- **THEN** CLI 立即返回持久接受结果及 Agent、任务身份，不等待完整回答；该 Agent 后续可继续接收工作

### Requirement: 常用协作能力预先进入提示词

系统 SHALL 在模型首次工作前说明协作时机、常用命令、主要参数、继承及工作区默认值、queue/steer 和结果回流方式。说明 SHALL 与已注册能力一致，低频参数通过帮助展开；不可用能力 SHALL 返回具体状态。

#### Scenario: 无需先搜索核心协作命令

- **WHEN** 新 Agent 首次获得任务
- **THEN** 模型已能知道如何 spawn、send、wait 及何时委派；完整参数可通过帮助查询，不宣传尚未注册的命令

#### Scenario: 系统和工具说明与能力同步

- **WHEN** 宿主组装首次提示词或发布新能力
- **THEN** 按 Pi 的短说明、使用规则和上下文分离方式组装，去重规则；说明中的命令、工具、默认值都能对应注册项，新说明以带来源和版本的记录追加

### Requirement: 默认继承当前可继承上下文

系统 SHALL 默认继承父 Agent 当前全部可继承上下文，并支持 none、最近若干轮、预算约束及模型补充的任务背景。继承 SHALL 取一致边界，不拆散完整消息与工具调用对，不把父 Agent 未完成调用交给子 Agent 执行。

#### Scenario: 父上下文已经压缩

- **WHEN** 父 Agent 使用默认继承创建子 Agent
- **THEN** 子 Agent 得到父当前摘要与保留内容，而非自动展开全部原始历史；身份和任务单独追加，后续双方历史独立增长

#### Scenario: 自定长度或不继承

- **WHEN** 模型选择 none、last:N 或上下文预算，并提交自己的背景说明
- **THEN** 系统按选择构造上下文并报告实际范围；容量不足或参数冲突明确报错，不静默截断调用对

### Requirement: 默认共享目录并允许显式 worktree

系统 SHALL 默认使用创建时父 Agent 的有效 cwd，子 Agent 独立保存该选择；显式 worktree SHALL 返回独立路径。系统 MUST NOT 自动合并或删除 worktree；不满足 Git 条件时 MUST NOT 静默退回共享目录。

#### Scenario: CLI 临时目录与父 cwd 不同

- **WHEN** 模型在命令中先 cd 到其他目录再默认 spawn
- **THEN** 子 Agent 使用父 Agent 的有效 cwd，不被临时 CLI 目录替代；父后续改变 cwd 不迁移已有子 Agent

#### Scenario: 选择 worktree

- **WHEN** 模型在可用 Git 仓库选择 worktree，或在非 Git 目录提出同样请求
- **THEN** 前者返回并保留独立路径，后者返回明确错误；均不悄悄改用共享目录

### Requirement: mailbox 区分接受与投递

系统 SHALL 持久保存消息身份、发送方、接收方、关联任务及投递方式；接受、进入上下文和完成处理 SHALL 为不同状态。进入上下文与更新消费位置 SHALL 原子完成或等效去重，重复请求 MUST NOT 产生第二条工作。

#### Scenario: 投递中途 core 退出

- **WHEN** 消息已接受，core 在投递接收方上下文期间退出
- **THEN** 恢复后按原消息身份补齐一次投递，历史与 mailbox 状态一致，用户和模型可查询长期往来

### Requirement: queue 与 steer 不绕过暂停

系统 SHALL 默认将忙碌 Agent 的 queue 消息留到当前任务结束；steer SHALL 在当前模型响应及所需工具调用结束后的下一次请求前投递。空闲未暂停时消息 SHALL 启动处理任务；人工暂停时两种消息 SHALL 只保存，不自动唤醒。

#### Scenario: 忙碌时分别发送两种消息

- **WHEN** Agent 正在执行工具时收到 queue 与 steer
- **THEN** queue 等本任务结束，steer 在下一安全模型请求边界进入；均不直接杀掉正在执行的工具

#### Scenario: 暂停时接收加急消息

- **WHEN** 已人工暂停的 Agent 收到 steer 或子任务结果
- **THEN** 消息持久保存，Agent 继续暂停，等待用户恢复

### Requirement: 完成结果自动回流且不会循环等待

系统 SHALL 将子任务 final 以一次结果消息默认 queue 回父 Agent，携带发送方、任务与原记录入口，不冒充用户。wait SHALL 直接读取指定任务终态与结果，不等待父 mailbox 消费；wait 超时 MUST NOT 取消被等待任务。

#### Scenario: 父任务正在 wait 子任务

- **WHEN** 子任务完成时父任务仍在 wait，结果同时准备 queue 回流
- **THEN** wait 直接取得子任务结果，不要求父任务先结束；同一结果经工具结果进入父上下文后记为已投递，不重复启动处理任务

#### Scenario: 只输出 CLI 结果不算上下文投递

- **WHEN** wait 已输出结果但父上下文尚未持久接受该结果
- **THEN** 系统不提前标记消息已投递，故障后仍能补齐；纯结果通知不强制自动回执形成回复循环

### Requirement: 调用者绑定与重试身份可追溯

系统 SHALL 为模型发起的 CLI 操作关联调用者、Agent、任务和工具调用，并允许稳定请求 ID 重试。归因绑定 SHALL 明确不构成同用户受信任代码之间的安全隔离。

#### Scenario: 协作命令连接中断

- **WHEN** 模型的 spawn 或 send 在服务已接受后发生连接中断
- **THEN** 重试可查询原请求结果，发送方和任务来源仍可确认，不重复创建 Agent 或消息

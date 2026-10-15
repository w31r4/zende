# Spec Delta

## Purpose

为模型提供能持续观察和控制的真实终端环境，而不是只有一次性命令返回。进程在前端断开后继续存在，输出和退出结果可追溯，文本修改有可校验的默认路径，同时保留系统命令的完整执行能力。

## ADDED Requirements

### Requirement: PTY 与 pipe 双路径

系统 SHALL 提供明确区分的 PTY 和 pipe 执行方式。Day1 的 PTY 运行环境 SHALL 要求 tmux；依赖缺失 SHALL 返回安装指引和不可用状态，MUST NOT 静默把 PTY 请求变成 pipe。

#### Scenario: 交互程序与普通命令

- **WHEN** 模型分别以 PTY 运行交互程序、以 pipe 运行普通命令
- **THEN** 前者具有终端语义，后者保留可区分的 stdout/stderr，两者报告真实执行方式

#### Scenario: tmux 不可用

- **WHEN** 请求 PTY 但 tmux 缺失或无法启动
- **THEN** 系统返回明确依赖错误，不把 pipe 结果当作 PTY 成功结果

### Requirement: 进程身份与持续控制

系统 SHALL 为命令保存稳定 `process_id`、工作目录、执行类型、状态与退出原因。初始等待到期的长命令 SHALL 返回进程身份；后续读取、stdin、resize 和 terminate SHALL 作用于原进程，不重新执行命令。

#### Scenario: 多次交互并终止进程

- **WHEN** 模型启动长命令后继续输入、读输出、调整 PTY 尺寸并终止
- **THEN** 全部操作关联同一进程，尺寸到达指定终端，终止返回实际退出状态或取消处理中状态

#### Scenario: 暂时没有输出

- **WHEN** 读取仍在运行但本次没有输出的进程
- **THEN** 返回空输出和运行状态，不将空输出认作进程退出

#### Scenario: 对 pipe 执行 resize

- **WHEN** 模型对 pipe 进程请求终端尺寸变更
- **THEN** 返回该操作不适用于 pipe 的明确错误，不伪报调整成功

### Requirement: 输出有读取位置和缺失说明

系统 SHALL 保存可追溯的输出位置、长度及截断状态，区分 PTY 字节和 pipe 文本流；返回片段 SHALL 指明后续读取位置。屏幕快照 MUST NOT 代替进程输出与退出记录，原始控制序列 MUST NOT 直接进入普通 UI 输出。

#### Scenario: 命令输出超过单次返回预算

- **WHEN** 命令产生超出工具结果预算的输出
- **THEN** 模型收到截断说明、可读取的输出引用和后续位置；已丢失区间明确标记，不能以省略内容冒充完整结果

### Requirement: 进程托管不依赖 core 连接

系统 SHALL 使活动命令不因 TUI 关闭或 core 连接中断而退出，并保存可核实的进程身份、输出位置和终态。重连 MUST NOT 只凭可复用的操作系统 PID 判断原进程仍在运行。

#### Scenario: core 重启后重连命令

- **WHEN** core 退出再启动而原命令仍存活
- **THEN** 系统关联原进程并补读输出；命令若已退出则取得原退出结果，身份无法验证时报告不确定而非误连其他进程

### Requirement: 读取与程序写入保持开放

系统 SHALL 允许模型通过执行命令选择读取、搜索、测试或脚本观察方式，也允许生成器、格式化器等程序写文件。读取 MUST NOT 强制经过固定 Read 工具；执行记录 SHALL 保留命令、关联身份、输出和退出结果。

#### Scenario: 自选读取与生成文件

- **WHEN** 模型通过搜索命令观察项目，再运行生成器写文件
- **THEN** 两种操作均可执行且有可追溯结果，不因未使用 Read 或 patch 而被拒绝

### Requirement: 默认 patch 可校验且不覆盖冲突

系统 SHALL 提供默认文本 patch 能力，支持新增、删除、更新和移动，校验旧内容及上下文并生成结构化变更结果。校验失败 MUST NOT 覆盖未知的新内容，SHALL 报告冲突位置或原因；该能力 MUST NOT 依赖仓库已经使用 Git。

#### Scenario: patch 与并发修改冲突

- **WHEN** patch 的预期上下文与当前文件不符
- **THEN** 该 patch 在应用前失败并说明冲突，现有文件内容保持不变

#### Scenario: 非 Git 目录修改文件

- **WHEN** 在非 Git 工作目录提交有效的文本 patch
- **THEN** 修改正常执行并返回文件级变更记录，不要求先创建 Git 仓库

### Requirement: 有依赖的进程操作有明确顺序

系统 SHALL 保持同一进程输入、同一目标变更及依赖调用的顺序；互不依赖的操作允许并行。timeout、取消、进程组终止和输出上限 SHALL 产生可查询状态，不属于权限审批。

#### Scenario: 连续输入与等待超时

- **WHEN** 同一进程收到有先后关系的输入，某次输出等待随后超时
- **THEN** 输入顺序保持，超时仅结束该次等待；进程是否终止由显式控制及其真实状态决定

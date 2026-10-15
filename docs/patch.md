# 文本 patch

`apply_patch` 是默认文本编辑工具，输入为 `{"patch":"..."}`，路径相对 Agent 的工作目录；也接受绝对路径和父目录路径。读取、搜索、生成器及格式化器继续使用命令工具。patch 不依赖 Git。

```text
*** Begin Patch
*** Add File: notes.txt
+new text
*** Update File: main.go
@@
-old line
+new line
 unchanged context
*** Update File: old-name.txt
*** Move to: new-name.txt
*** Delete File: obsolete.txt
*** End Patch
```

## 匹配

新增行以 `+` 开头，删除行以 `-` 开头，上下文以空格开头。`@@ exact anchor` 指定之前的一行；后续 hunk 从上次匹配之后查找。只有新增行的 hunk 追加到文件尾。最后一个 hunk 可用 `*** End of File` 限定匹配到 EOF。空 Add 创建空文件；Move 可单独移动，也可携带更新 hunk。

旧行与上下文精确匹配。未找到或匹配不唯一时返回冲突，应补充上下文；不会忽略空白后猜位置。Add 和 Move 的目标必须不存在。同一 patch 的重复、别名或父子目标被拒绝。

输入必须是 UTF-8 文本，不含 NUL。patch 支持 LF/CRLF 分隔，文件未修改行保留原换行；新增行沿用附近换行风格，修改未带结尾换行的末行时保留该风格。更新保留普通权限位，包括可执行位；单文件通过临时文件替换，不保留 inode、硬链接关系、所有权或扩展属性。

目录软链接会解析到实际目录，文件本身的软链接、目录和特殊文件明确报错，需要用命令处理。这是文本编辑规则，不是 workspace 沙箱。

## 校验与结果

先解析并校验全部目标，再次核对快照后开始写入。预校验失败不创建目录或修改文件。运行中的 patch 在同一 core 内顺序执行；每个文件发布前再核对身份和内容。新增与移动目的地使用不覆盖发布，期间出现同名文件也会失败。

结果含逐文件状态、前后 SHA-256、hunk 行范围、`destination_written`、`source_removed`、`durable`，以及已创建目录和未清理临时文件。行范围使用原文件/结果文件的一基行号，数量为零表示插入或删除。`after_sha256` 描述计划结果，是否生效要同时看状态和写入标记。

写入阶段失败会停止后续文件，保留已生效改动。移动先发布新文件再删除旧文件；删除失败时两者都保留，结果为 partial。目录同步失败时实际可见范围仍报告，但持久性未知，任务进入待核实。不要假定跨文件回滚，重试前检查原结果和文件。

外部编辑器、shell 和其他 core 不受该进程内顺序控制。更新/删除的最后一次核对与文件操作之间仍有竞争窗口；不能宣称对任意外部写入者提供跨文件事务或文件内容 CAS。需要隔离并行编辑时使用独立工作目录。

## 限额与恢复

单个 patch 最多 1 MiB、128 个文件、1024 个 hunk；单文件最多 8 MiB、200000 行，所有旧/新内容合计最多 32 MiB。输入路径和解析后的文件/新目录路径各限 32 KiB，保证结果可完整进入模型上下文。超限在写入前报错。

core 在派发前保存调用意图。工具在 profile 状态目录的 `patches/<operation_id>.json` 保存 pending，再应用补丁；完整结果以原子替换和 fsync 保存后才返回 core。记录只含参数指纹和结果，不重复保存补丁正文；指纹绑定 Agent、Task、cwd 和原参数。同一操作 ID 的重复执行返回原结果。

重启核实只读原记录。completed 成功或已知部分失败可补入原上下文，即使文件后来被其他程序修改，也只返回历史结果，不重新写文件。pending、缺失、损坏、参数不符或目录同步失败保持 unknown；文件看起来符合预期不足以证明原调用完成。查询用 `agent recovery`，再次核实用 `agent recover`，新输入和新调用 ID 均不解除待核实状态。底层 parser/engine 仍是无持久状态的文件操作实现。

## 实现与验证

实现为 `internal/patch/`、`internal/tool/patch.go` 和 `internal/tool/patch_recovery.go`。语法参考本地 Codex 提交 `7219fd735bef2f9cfd0363fecdbbb212e3df5255` 的 `codex-rs/apply-patch/src/parser.rs`；hunk 查找与换行处理参考 `file_update.rs`。Zende 按自己的 spec 使用全部目标预校验、精确唯一匹配及目标冲突保护。

```sh
go test ./internal/patch ./internal/tool
go test ./internal/runtime -run 'TestLoopPatchConflictRecoveryAndShellGenerator|TestIncompletePatchCallNeverWrites' -count=1
```

模型部分使用确定性夹具；文件和 shell 进程实际运行于临时目录。故障测试覆盖预校验冲突、快照改变、发布失败、移动删除失败及目录同步失败。

`TestPatchReceiptFailureBoundariesNeverReplay` 覆盖意图写入失败、文件已改后中断、结果写入失败；`TestRecoveryPatchReceiptContinuesWithoutReapplying` 重开数据库，验证原任务续接、已知冲突回填，以及记录缺失/版本不符时阻止新输入执行。恢复查询不创建目录，不调用文件编辑器。

# 编写插件

Zende 插件是独立程序，通过 stdin/stdout 的逐行 JSON-RPC 与 Core 通信，日志写 stderr。插件按当前用户权限执行，可以访问该用户的文件、网络和环境；进程分离提供生命周期管理与故障隔离，不提供沙箱。

## 运行最小示例

[plugin-echo](../examples/plugin-echo/main.go) 只依赖 Go 标准库，同一个 echo 能力分别注册为 tool 和 command，支持进度、延迟、并发和确认取消。主程序可复制到仓库外单独构建。它不访问网络，也不修改业务文件。

准备本仓库和 Go 1.26 工具链（本机验证为 1.26.5），在项目根目录构建：

```sh
go build -o bin/zende ./cmd/zende
go build -o bin/zende-plugin-echo ./examples/plugin-echo
```

单独复制 main.go 后，也可在其目录执行 `go build -o zende-plugin-echo main.go`。测试会把它复制到临时目录，关闭 module/workspace 模式构建并运行该二进制。

复制 [manifest.json](../examples/plugin-echo/manifest.json) 到自己的插件目录，将 command 改为刚构建的 `zende-plugin-echo` 的绝对路径。保持 id/version/capabilities 与示例程序一致；可执行内容变更时同时更新程序与 manifest 的版本。

用独立 profile 验证，路径占位符换成已有目录。core start 在后台运行，返回后可在同一终端继续。下面的 AGENT 取 `agent create` 返回的 agent_id，RESOURCE 取注册返回的 resource.id，REF 取目录 definitions 中 kind=command、name=echo 的 ref：

```sh
./bin/zende --profile plugin-demo core start
./bin/zende --profile plugin-demo agent create --cwd /absolute/workspace
./bin/zende --profile plugin-demo --request-id install-echo resource register AGENT /absolute/plugin/manifest.json --kind plugin
./bin/zende --profile plugin-demo plugin catalog AGENT RESOURCE
./bin/zende --profile plugin-demo plugin instances AGENT RESOURCE
```

将调用参数保存为 `arguments.json`：

```json
{"text":"hello 中文","delay_ms":30000}
```

```sh
./bin/zende --profile plugin-demo --request-id echo-1 plugin command AGENT RESOURCE REF --file arguments.json
./bin/zende --profile plugin-demo plugin inspect AGENT echo-1
./bin/zende --profile plugin-demo plugin progress AGENT echo-1
./bin/zende --profile plugin-demo --request-id cancel-echo-1 plugin cancel AGENT echo-1
./bin/zende --profile plugin-demo plugin inspect AGENT echo-1
./bin/zende --profile plugin-demo plugin result AGENT echo-1
```

命令返回持久接受记录；重复 inspect，直到取得 completed/failed/cancelled 或 unknown。取消先返回 cancelling，不能立即当作终态。示例从收到调用开始等待 30 秒，期间确认取消返回 cancelled；先完成则保留 completed。确认终态后再读 result。把 delay_ms 改为 0，用新操作 ID 调用可观察正常结果。

result 的 text 是保留的 CallResult JSON；more=true 时用 `plugin result AGENT echo-1 --offset NEXT` 继续读取（保留同一 profile），NEXT 取上一页 next，直到 more=false。按序拼接 text，其中 completion.output.text 为回显正文。

模型调用 tool 时使用已注册的 `plugin_invoke`，参数为 resource_id、kind=tool 的 tool_ref 和 arguments。插件定义按需查询，工具与命令的引用不能互换。插件不需要自己创建 TUI。

## 协议 v1

本节描述 Core 与插件之间的 stdio 协议；它与客户端连接 Core 的 Unix socket 接口不同。帧为 UTF-8 JSON 对象，以换行结束，不带 Content-Length。请求使用字符串 id；响应只有 jsonrpc、id 和 result/error 之一，通知只有 jsonrpc、method、params。stdout 的额外日志、错关联、非法字段或越限都会使连接失败。

### 初始化与目录

| 宿主请求 | 插件响应的 result |
| --- | --- |
| `plugin.initialize`，params 含 protocol_versions、host、plugin、instance_id、capabilities、limits | protocol_version、plugin `{id,version}`、capabilities；身份和能力必须与 manifest 一致 |
| `plugin.describe`，params 为 `{kinds:[...]}` | 按声明返回 tools、commands、providers、resources、events 数组；只填写已声明的能力 |
| `plugin.shutdown` | null；随后退出，清理自己管理的资源 |

当前协议版本为 1。启动配置与初始化字段见 [manifest.go](../internal/resource/plugin/manifest.go)。插件应实际实现所选版本与能力，不能只回显宿主给出的值。初始化、发现和 manifest 复读全部成功后，Core 才发布 ready。

tools/commands 的每项包含 name、description、input_schema，以及可选 output_schema；输入和输出 schema 都必须描述 object。实例生命周期内目录保持固定，更新须准备新实例。宿主根据 manifest 和完整定义生成 ref，再在调用中传回；插件用 name 路由能力，不自行生成宿主的引用。

### 调用、进度与取消

宿主发送 `plugin.tool.call` 或 `plugin.command.call`，params 包含 operation_id、definition_ref、name、scope 和 arguments。scope 带 cwd 及按需存在的 agent_id/task_id。需要执行文件操作时使用 scope.cwd，不在并发请求中全局 chdir。

JSON-RPC id 只关联本次交换，operation_id 关联持久操作。进度通知同时带两者，sequence 从 1 连续递增：

```json
{"jsonrpc":"2.0","method":"plugin.progress","params":{"request_id":"rpc-1","operation_id":"echo-1","sequence":1,"value":{"stage":"waiting"}}}
```

成功响应原请求，不能用通知替代终态：

```json
{"jsonrpc":"2.0","id":"rpc-1","result":{"operation_id":"echo-1","state":"completed","output":{"text":"hello"}}}
```

已确认业务失败仍使用 result，state=failed，带 error `{kind,message}`；可选 output 必须满足原定义。JSON-RPC error 表示协议层失败，不足以证明操作未生效，宿主会将已经派发的调用保留为 unknown。

宿主请求取消时发通知：

```json
{"jsonrpc":"2.0","method":"plugin.cancel","params":{"request_id":"rpc-1","operation_id":"echo-1"}}
```

取消通知本身没有响应。插件停止该项工作后，用原请求 id 返回 state=cancelled；若工作已经完成，则保留 completed。已发终态后不再发送进度或第二个响应。宿主最多等 5 秒确认，超时关闭故障实例，相关未完成调用保留 unknown，同实例其他调用也可能受影响。取消不能承诺回滚外部效果。

stdin 必须持续可读；等待某个调用时仍应接收取消和其他请求。完整帧串行写入 stdout，各调用的进度和结果按各自身份关联。示例用单写入循环处理这些顺序。

### 其他能力

| 能力 | 目录与通信 |
| --- | --- |
| provider | providers 定义 name、description、models；`plugin.model.stream` 接收公共 request，发送 `plugin.model.delta` 通知，最后响应原请求 |
| skill/prompt | resources 定义 name、description、kind、entry、files；`plugin.resource.read` 按 ref/path 返回 ref/path/data/executable，data 为 base64 |
| Core 事件 | events 定义 direction=core；`plugin.events.open` 打开，`plugin.events.deliver` 收到保留原身份的 Core 事件，确认 through 游标，close 确认关闭 |
| 插件事件 | events 定义 direction=plugin 和 object schema；open 后按 `plugin.events.read` 返回有连续 sequence 的数据页；close 确认关闭 |

provider 的通知 params 为 request_id、operation_id、sequence、delta；operation_id 等于公共 request.request_id。公共模型字段见 [model.go](../internal/model/model.go)，wire 封装见 [plugin/model.go](../internal/resource/plugin/model.go)。response.end 和合法 completed 响应都收到才表示传输成功；可见 final 仍必须显式分类，工具参数完整后才执行。取消复用 plugin.cancel。

资源文件清单包含路径、字节数、SHA-256 和执行位。宿主核对全部内容后发布，读取目录不会自动应用正文或执行脚本。目录类型见 [capability_catalog.go](../internal/resource/plugin/capability_catalog.go)；接入用法见 [插件模型](./development.md#插件模型)与[插件 skill/prompt](./development.md#插件-skillprompt)。

事件请求和响应字段见 [events.go](../internal/resource/plugin/events.go)，[双向事件说明](./development.md#双向事件传输)列出游标规则和 CLI。插件来源的 read 不消费事件，旧游标须返回相同数据或明确缺口；closed=true 仍需保留最后一页。Core 来源须保持原 agent_id/seq，不把投递确认重新发成运行事件。所有 ack 都须显式带 subscription_id、cursor、state，包括零游标。插件值作为带来源的数据保存，通过查询读取。

## 换版、退出与结果核实

更新先准备新实例，ready 后才发布。旧调用、模型流和订阅保留旧实例，最后一个引用释放后回收；失败候选保留上一版。Core 重启保留目录和已收结果，显式 resource refresh 才重新准备插件进程。

| 观察结果 | 下一步 |
| --- | --- |
| completed / failed / cancelled | 读取原结果；终态表示确认的执行结果，外部效果仍需按业务解释 |
| dispatched / cancelling | 查询原调用与进度；CLI 返回或客户端断开都不代表结束 |
| unknown | 查看原实例、操作 ID、进度、证据及业务产物；刷新或换操作 ID 不会授权重做 |
| persistence_failed | 查询失败保留故障标记；修复存储后核实原记录，不重新提交副作用 |

如果结果可能丢失，插件最好提供只读的业务查询渠道，并在自身业务中持久记录 operation_id。宿主不保证任意外部服务 exactly-once；当前没有“确认一下就当成功”的通用入口。没有可验证结果的插件调用或订阅会继续保持 unknown。关联模型工具可用 `agent recovery` 查询，`agent recover` 只重新核实已有证据，不强制重放或人工改写终态。

插件崩溃只影响该实例；同实例的其他未完成调用也可能失去结果，其他 Agent 的独立实例继续。宿主关闭进程组不能撤销已落盘或远端生效的操作。已保留历史、进度、结果和订阅数据继续可查。

## 限额与验证

`plugin.initialize.params.limits` 给出当前 wire 限额；`plugin instances AGENT RESOURCE` 查询实例及 Core 的活动/保留限额。默认单实例 8 个并发 RPC、帧 8 MiB、取消确认 5 秒；模型请求帧上限另为 32 MiB。完整限额与字段见 [Limits](../internal/resource/plugin/manifest.go)，达到限制明确报错，不引入审批。

```sh
go test -race ./examples/plugin-echo -count=3
go test ./internal/cli -run '^TestPluginCatalogThroughBinaryCLI$' -count=1
go test -race ./internal/runtime -run '^TestPluginEffectSurvivesExitWithoutReplay$' -count=3
go test -race ./internal/resource/plugin ./internal/runtime ./internal/store -run 'TestPlugin|TestCoreStartupHoldsUnfinishedPluginCall'
```

第一项在临时目录构建独立示例，验证两类调用、并发、进度、取消和关闭；测试二进制随临时目录清理，手动注册使用开头的构建产物。第二项用真实 CLI/Unix socket 注册并执行示例，同时检查五类能力目录、资源读取和双向订阅；provider 模型循环由全套测试另行覆盖。第三项在插件 fsync 产物后、响应前退出，检查效果只出现一次、其他 Agent 完成任务，数据库重开和显式刷新不重放。全套测试覆盖配置/协议失败、换版、断流和持久化故障；全部使用本地进程与隔离目录，外部模型兼容性和本机部署另行验收。

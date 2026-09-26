# 示例

每个示例是一份**最小可运行配置**加一段**能自证的脚本**：脚本说明它要证明什么、期望观察到
什么、实际观察到什么。克隆仓库后不需要任何凭据就能看到结果，而同一份脚本又被 `make check`
守着，因此示例不会在功能演进后变成过期的文档。

## 一条命令跑完

```sh
go run ./examples/run                  # 列出可用示例
go run ./examples/run quickstart       # 一条命令跑完并打印每一步的期望与实际
```

输出形如：

```
✓ 1. 主用失败后换回退候选
     期望  客户端拿到 200；模拟上游收到两次尝试，先是失败的 relay-a（mock-key-5xx），后是成功的 relay-b（mock-key-ok）
     实际  POST /v1/chat/completions → 200  {"choices":[…"content":"chatmock: shared"…]
     实际  上游 #1  openai_chat /v1/chat/completions  model=shared  key=mock-key-5xx  经 Authorization  非流式  故障=5xx
     实际  上游 #2  openai_chat /v1/chat/completions  model=shared  key=mock-key-ok  经 Authorization  非流式
```

哪一步判定不成立就停下，把该步的期望、实际与 nova 日志一起打出来，并以非零退出。
它自己起模拟上游、自己起 nova、自己收尾，因此不需要另开终端，也不需要第二条命令。

不想要这层包装时，每个示例的 `Novafile` 顶部就写着手工运行的两条命令（先起模拟上游、
再起 nova，命令里已经把状态目录指到临时目录）；以 quickstart 为例：

```sh
go run ./examples/chatmock
XDG_STATE_HOME=$(mktemp -d) NOVA_CLIENT_KEY=example-client-key go run ./cmd/nova run -c examples/quickstart/Novafile
```

想自己动手试两下时加 `-serve`：跑完不退出，保持服务并打印可直接粘贴的 curl（模型名从
网关自己的 `GET /v1/models` 取，因此与这个示例实际暴露的模型一致），按 Ctrl-C 结束。

```sh
go run ./examples/run quickstart -serve
make example NAME=quickstart ARGS=-serve
```

## 示例清单

| 示例 | 演示什么 | 主文档章节 |
|---|---|---|
| `quickstart` | 最小的一条路：鉴权、非流式与流式转发、模型清单、未知模型 | [配置](../README.md#配置)、[客户端鉴权](../README.md#客户端鉴权) |
| `convert` | 协议转换矩阵：客户端四种协议 × 上游四种协议，各含流式 | [协议转换](../README.md#协议转换) |
| `route-fallback` | 主用失败换回退、`route` 重排优先级、`balance` 加权轮转 | [跨渠道分摊与回退](../README.md#跨渠道分摊与回退) |
| `accounts` | 一个渠道多个账号：可重试才换账号、不可重试不换、`balance` 轮转、`acct #N` | [一个渠道多个账号](../README.md#一个渠道多个账号) |
| `discover` | `discover` / `allow` / `deny` / `expose` 与显式 `model` 的关系 | [模型自动发现与过滤](../README.md#模型自动发现与过滤) |
| `gemini` | Gemini 双向：路径上的模型名与动作、`{model}` 引号、`x-goog-api-key` | [Gemini 端点](../README.md#gemini-端点) |
| `real-providers` | 接真实上游的配置骨架（四条渠道、四种协议、凭据全在环境变量） | [环境变量与拆分文件](../README.md#环境变量与拆分文件) |

六个会启动服务的示例端口固定，互不冲突，也不占用 nova 的缺省值 8080 / 2026：

| 示例 | 模拟上游 | `listen`（客户端请求进来） | `admin`（`nova reload` 投递重载） |
|---|---|---|---|
| `quickstart` | 18080 | 18081 | 18082 |
| `convert` | 18083 | 18084 | 18085 |
| `route-fallback` | 18086 | 18087 | 18088 |
| `accounts` | 18089 | 18090 | 18091 |
| `discover` | 18092 | 18093 | 18094 |
| `gemini` | 18095 | 18096 | 18097 |

`listen` 是客户端请求进来的那个地址；`admin` 只服务 `nova reload`，把「用这份配置重新装配」
投递过去，因此它与 `listen` 是两个端口。

唯一例外是 `real-providers`：它是给真实部署用的骨架，因此写的是 nova 的缺省端口 8080 / 2026，
而且不启动服务（只跑 `config check`）。本机已有一个 nova 在跑时，手工跑它会得到「端口被占用」。

## 模拟上游：chatmock

示例不接真实上游，全部指向 `examples/chatmock`。它同时是一个独立命令：

```sh
go run ./examples/chatmock -addr 127.0.0.1:18080
```

- **四种上游线协议各一个端点**：`/…/chat/completions`、`/…/responses`、`/…/messages`、
  `/v1beta/models/<模型>:generateContent` 与 `:streamGenerateContent`，流式按各协议的
  SSE 形状分片下发；
- **清单端点** `/…/models`：带 `anthropic-version` 头时按 Anthropic 形状回答，缺省按 OpenAI 形状，
  与 nova 自己的 `GET /v1/models` 同一口径；
- **响应确定**：正文回显本次收到的上游模型名，用量固定为 11 / 7。于是断言可以精确到
  「nova 把哪个上游模型名发了出去」，而不是「看起来收到了一个回复」；
- **记录每一次请求**：协议、路径、上游模型名、是否流式、凭据与承载它的头名。
  「回退到了哪条渠道」「用了哪条账号」「地址末段的动作换没换」都由这份记录回答——
  响应正文只反映最终结果，回答不了回退链上的过程；
- **故障由凭据后缀注入**：

  | 后缀 | 模拟上游的行为 | nova 的处置 |
  |---|---|---|
  | `-429` | 回 429 | 上游限流，**可重试**（换账号、换渠道） |
  | `-5xx` | 回 503 | 上游不可用，**可重试** |
  | `-slow` | 不回包，由端点 `timeout` 触发超时 | 上游超时，**可重试** |
  | `-bad` | 回 400 | 上游拒绝请求，**不可重试** |

  按凭据注入而不是按路径或查询串，是刻意的：账号池与渠道回退要演示的正是
  「同一个端点下、不同凭据表现不同」，只有凭据天然带着这个区分度。

## 目录结构

```
examples/
  run/                  交互式运行：go run ./examples/run <示例名>
  chatmock/             模拟上游的命令行外壳
  internal/
    chatmock/           模拟上游：四协议端点、清单、故障注入、请求记录
    stack/              起模拟上游与 nova 的进程管理（交互式运行与校验共用）
    demo/               脚本的执行与报告：Step、Scenario、期望/实际的排版
    harness/            把脚本接到 go test 上（失败即测试失败）
  <示例名>/Novafile     示例配置
  <示例名>/scenario.go  该示例的脚本：发什么请求、期望什么、怎么判定
  <示例名>/*_test.go    该示例的校验：harness.Run(t, Scenario())
  gemini/Novafile.invalid-url  故意写错的配置，供 gemini 的第五步断言加载期报错
  real-providers/.env.example
```

进程管理只有一份实现（`internal/stack`），脚本也只有一份（各示例的 `scenario.go`）：
交互式运行与校验走的是同一个 `Step` 列表，因此「跑一遍看到的」与「make check 守住的」
不会漂成两件事。

## 校验

```sh
make check-examples    # 只跑示例
go test ./examples/... # 同上
go test ./examples/quickstart/ -v
```

示例的校验是普通测试，因此 `make test`（`go test -race ./...`）与 `make check` 一并覆盖它们，
CI 无需额外配置。每个测试文件只有一行 `harness.Run(t, Scenario())`——断言在 `scenario.go` 里，
不另抄一份。校验里有三个刻意的选择：

- **起的是真二进制**（`go build` 到临时目录），不是进程内调用 `internal/gateway`：
  示例展示的是「配置文件 → 命令行 → HTTP」这条链路；
- **端口与模拟上游地址从示例配置里读出来**，不在测试代码里再写一遍。因此示例配置里的
  `listen`、`admin`、`url` 必须写成字面量（不写 `{env.NAME}`）——这本来就是示例该有的样子；
- **判定失败即停并给出该步的报告**：期望与实际并排打出来。后面的步骤常建立在前一步产生
  的事实上，继续跑只会刷出一串同源错误。

`examples/run` 与示例校验的运行期落点都在临时目录（`XDG_STATE_HOME`、`XDG_CONFIG_HOME`
指向临时目录，进程退出即删）：开发机上通常有一个正在运行的 nova，它用的是
`~/.local/state/nova/stats.db`，不隔离就会把示例流量写进那份真实统计里。
手工运行的命令（各 `Novafile` 顶部那两行）里也带了同样的前缀，因此照抄也不会污染它。

`real-providers` 是唯一不被跑通的示例：它连真实上游、需要真实凭据。
它的脚本因此是 `NoServe`：只构建二进制并跑 `nova config check`（该命令按设计不联网，
连发现型端点的清单地址也不请求），不需要凭据，也不监听端口。

## 约定

- **配置只留本示例需要的那几行**。逐条指令的完整语义在仓库根目录的
  [`Novafile.example`](../Novafile.example)，示例不复述它，只指回去；
- **客户端凭据写 `{env.NOVA_CLIENT_KEY}`**，取值由运行方注入（交互式运行与校验都用
  `example-client-key`，`real-providers` 用占位值）；示例不演示「不写 `client_key`」的形态——
  默认配置应当安全；
- **手工路径要自己隔离状态目录**：`examples/run` 与示例校验会把 `XDG_STATE_HOME` 指向临时目录，
  手工按每个 `Novafile` 顶部那两行跑时，命令里也带了这个前缀——去掉它，统计就会写进真实运行用的
  `~/.local/state/nova/stats.db`（落点见[统计](../README.md#统计)）；
- **上游 `api_key` 写死为故障开关**（如 `mock-key-5xx`）。它是模拟上游的开关而不是凭据，
  真实渠道一律写 `{env.NAME}`，`real-providers` 就是那个形态；
- **端口取 180xx 段**（`real-providers` 除外，见上），避开 8080 与 2026：开发机上常有真的 nova 在跑，
  抢端口得到的是「配置写错了」的错觉。

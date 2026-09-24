# nova

大模型 API 转发网关。一个二进制、一份配置文件、几条命令。

## 当前状态

网关可以转发：客户端请求经鉴权后按对外模型名选路，需要时在三种线协议之间转换，
前一条候选失败时按配置里的声明顺序回退，流式与非流式都支持。

尚未实现的是可观测性的**采集侧**：上游尝试记录与访问日志已经接上（见「日志」一节），
但还没有导出为指标或追踪。`log_level` 与 `log_format` 共同决定这些记录怎么输出。

## 安装

```sh
make build-binary                    # 产出 bin/nova
sudo make install                    # 安装到 /usr/local/bin/nova
PREFIX=$HOME/.local make install     # 或装到用户目录
```

构建产物只有两个出口：`bin/nova` 与 `$(PREFIX)/bin/nova`。

## 配置

缺省读取 `${XDG_CONFIG_HOME:-$HOME/.config}/nova/Novafile`。用 `-c` 或环境变量
`NOVA_CONFIG` 指向别处（`-c` 优先于环境变量）：

```sh
nova run -c ./Novafile
NOVA_CONFIG=/etc/nova/Novafile nova run
```

Novafile 的语法沿用 Caddyfile 的取舍：**指令直接写在顶层，一行一条；只有需要把一个
地址和它提供的模型绑在一起时，才用花括号。**

```
version 1
listen 127.0.0.1:8080
admin 127.0.0.1:2026
log_level info
log_format text
client_key {env.NOVA_CLIENT_KEY}

provider openai {
    api_key {env.OPENAI_API_KEY}
    url https://api.openai.com/v1/chat/completions
    model gpt-5
    model gpt-5-mini
}
```

完整规格与逐条注释见 [`Novafile.example`](Novafile.example)。

顶层指令：`listen`（缺省 `127.0.0.1:8080`，只绑回环）、`admin`（缺省 `localhost:2026`）、
`log_level`（缺省 `info`）、`log_format`（缺省 `text`）、`client_key`（可写多条；不写则不鉴权，
并在绑非回环时告警）、`import`（把另一个文件拼进来）。

### 上游渠道

`provider <名>` 描述一条上游渠道。名字只用于日志与错误定位，不参与选路。
凭据 `api_key` 写在 provider 一级，本块的**所有端点共用它**；写多条就是多个账号，
见下面的「一个渠道多个账号」。

一条端点由三件事构成：**地址、协议、它提供哪些模型**。

| 指令 | 位置 | 必填 | 说明 |
|---|---|---|---|
| `url` | 端点 | 是 | 完整地址，含协议段与端点路径；Gemini 端点里用 `{model}` 指代模型名 |
| `protocol` | 端点 | 否 | `openai_chat` / `openai_responses` / `anthropic_messages` / `gemini`；省略时从 `url` 末段推导，写了则必须与推导一致 |
| `timeout` | 端点 | 否 | 单次上游调用超时，缺省 `60s` |
| `model <对外名> [<上游名>]` | 端点 | 至少一条 `model` 或一条 `discover` | 对外名是客户端请求里要匹配的名字；省略上游名时，发往上游的名字与对外名相同 |
| `discover [<地址>]` | 端点 | 否 | 模型来自上游清单；省略地址时从 `url` 推导；Gemini 端点不支持 |
| `allow <模式>` | 端点 | 否，可多条 | 白名单，只保留命中的发现模型 |
| `deny <模式>` | 端点 | 否，可多条 | 黑名单，排除命中的发现模型 |
| `expose <上游模式> <对外名模式>` | 端点 | 否，可多条 | 把清单里的上游 id 改写成对外名，第一条命中即生效 |

`model` 的两个记号都按字面量使用：`model sensenova/* *` 的含义是「对外名就叫 `sensenova/*`、
发往上游时写 `*`」，不是通配匹配（名字里含 `*` / `?` 时加载会记一条提醒）。要按模式筛上游清单，
用 `discover` 下的 `allow` / `deny`。

一个 provider 可以有一条**默认端点**和任意多条**具名端点**：

- 端点指令直接写在 provider 一级 → 描述那条默认端点（单端点时最省事）；
- 写成 `endpoint <地址> { ... }` → 描述另一条端点，块头就是这个端点的地址；
- 一个 provider 至多一条默认端点，所以「两条端点各自带模型」必须用 `endpoint` 块。

```
provider relay {
    api_key {env.RELAY_API_KEY}

    url https://relay.example.com/v1/chat/completions
    model gpt-5

    endpoint https://relay.example.com/v1/messages {
        protocol anthropic_messages
        model claude-sonnet-4 claude-sonnet-4-20250514
    }
}
```

### Gemini 端点

Gemini 把模型名与动作（是否流式）都写在请求路径上
（`/v1beta/models/{model}:generateContent` 与 `:streamGenerateContent?alt=sse`），
请求体里没有这两项。端点的 `url` 因此用 `{model}` 指代本次请求的模型名，**并要加引号**——
不加引号时 `{` 会被词法器当成块开启，取值被切成多段。

```
provider gemini {
    api_key {env.GEMINI_API_KEY}

    url "https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent"
    model gemini-2.5-flash
    model gemini-2.5-pro
}
```

网关按本次请求是否流式把地址末段改成 `:generateContent` 或 `:streamGenerateContent?alt=sse`，
凭据以 `x-goog-api-key` 注入。`discover` 本版不支持 Gemini 端点：清单响应的形状不同，
且地址里带占位符、推不出清单地址；模型用 `model` 逐条声明。

### 一个渠道多个账号

`api_key` 可以写多条，一条一个账号：它们共用本块的地址、协议、超时与模型声明，
只有凭据不同。加一个账号就是复制一行。

```
provider openai {
    api_key {env.OPENAI_KEY_1}
    api_key {env.OPENAI_KEY_2}
    api_key {env.OPENAI_KEY_3}

    url https://api.openai.com/v1/chat/completions
    model gpt-5
}
```

不写 `balance` 时按声明顺序调度：前面的账号失败且可重试，才换后面的。

写 `balance` 时按权重轮询分摊：每次请求把起点前移一个位置，链尾仍保留全部账号，
因此分摊在正常情况下生效、故障时照常回退。权重写在 `api_key` 的取值后面，省略即 `1`；
下面的 `3` 表示每轮里第一个账号轮到三次。

```
provider openai {
    api_key {env.OPENAI_KEY_1} 3
    api_key {env.OPENAI_KEY_2}
    balance

    url https://api.openai.com/v1/chat/completions
    model gpt-5
}
```

写了权重却没写 `balance`（权重没有作用）、单账号写了 `balance`（没有可分摊的对象），
两种都记一条提醒。

**账号池文件**：账号多了就单独放一个文件，用 `import` 拼进来。被导入的文件就是一段
provider 块体，因此走同一套词法、注释与 `{env.NAME}`，写错的一行也报出池文件自己的位置。

```
provider openai {
    import accounts/openai-*.nova

    url https://api.openai.com/v1/chat/completions
    model gpt-5
}
```

```
# accounts/openai-01.nova
api_key {env.OPENAI_KEY_1}
api_key {env.OPENAI_KEY_2}
```

一条 glob 内的文件按文件名字典序拼接，多条 `import` 按书写先后拼接，因此文件名与书写
顺序决定账号顺序。账号池是渠道级的，`import` 写在 provider 块内即可，与端点无关。

`config check` 与启动横幅在多账号时多输出一行账号池摘要（账号数与调度口径）。

### 跨渠道分摊与回退

同一个对外名落在多条渠道上时，缺省按渠道的**声明顺序回退**（前面那条失败才换下一条）。
要让它们分摊、或显式指定失败后切到哪条渠道，用顶层 `route` 块：

```
route deepseek* {
    balance                    # 可选：主用候选按权重轮询分摊

    provider relay-a 3         # 主用候选；渠道名后可跟权重，省略即 1
    provider relay-b 1
    fallback relay-c           # 主用候选全部失败之后才轮到
}
```

候选按三段的固定顺序拼成一条链：

| 段 | 来源 | 写 `balance` 时 |
|---|---|---|
| 主用 | `provider` 行，按书写顺序 | 按权重轮转起点 |
| 回退 | `fallback` 行，按书写顺序 | 不参与轮转 |
| 兜底 | 规则没提到的渠道，按声明顺序 | 不参与轮转 |

「主用全部失败才切换」因此是结构上的事实：`relay-a` 与 `relay-b` 的每一条账号都试过之后，
才轮到 `relay-c`。一行可以列多个渠道名：`provider relay-a relay-b`（各权重 1）；
权重写在它所属渠道名之后，所以渠道名不能是纯数字。同一个渠道的多条端点候选作为一个
整体移动：先试完这条渠道的全部端点，再换下一条。

不写 `balance` 时主用段按书写顺序回退，用来重排优先级。

块头是对外名模式：不含 `*` / `?` 时就是那一个名字，含通配时匹配一组名字。
**匹配大小写敏感**（对外名本身是精确匹配的，模式宽于它就会出现「规则命中了、请求却
找不到模型」），两端隐式锚定，`*` 可以跨 `/`。多条规则按声明顺序取**第一条命中**，
更具体的写在前面即可。

命中规则的模型不再做「同协议候选优先」那条隐式排序：规则写下的顺序就是最终顺序。
跨协议的候选同样是被明确列出的等价来源，把它推到链尾会让分摊失效。

一条规则没有命中任何对外名、或规则里的某个渠道没有提供匹配该模式的模型，加载会各记
一条提醒。这两件事只能在装配期回答，因为对外名集合包含 `discover` 来的那些。

### 模型自动发现与过滤

中转站与聚合网关单条端点下有几十到几百个模型，逐条抄写既容易抄错，也会与上游真实可用的
集合随时间漂移。`discover` 把端点的模型集合交给上游清单接口，`allow` / `deny` 决定其中
哪些暴露给客户端：

```
provider aggregator {
    api_key {env.AGGREGATOR_API_KEY}

    url https://open.bigmodel.cn/api/coding/paas/v4/chat/completions
    discover
    allow gpt-5*
    allow claude-*
    deny *-preview

    # 需要改写上游名的模型仍然显式声明，且不受 allow / deny 影响。
    model gpt-5 gpt-5-2025-01-01
}
```

**清单地址的推导**只做一次后缀裁剪加一次固定拼接：裁掉协议端点段，再拼 `/models`，
版本根原样保留。`/v1/chat/completions`、`/v1/messages`、`/api/coding/paas/v4/chat/completions`
因此都推出各自版本根下的 `/models`，不假定任何一家供应商的版本根写法。清单不在这条规则上时
显式写地址。

**过滤模式**匹配整个模型 id，两端隐式锚定：`gpt*` 是「以 `gpt` 开头」，`openai/gpt-4o` 要用
`openai/gpt-*`。`*` 可以跨 `/`（模型 id 里的斜杠是名字的一部分，不是路径分隔符），`?` 匹配
一个字符，匹配不区分大小写。一条 `allow` 都不写表示全部保留（启动时会记一条提醒）；
`deny` 优先于 `allow`。

**改名（`expose`）**把清单里的上游 id 按模式改写成对外名，可写多条、第一条命中即生效：

```
discover
allow *
expose * sensenova/*        # 上游 kimi-k3 → 客户端可见 sensenova/kimi-k3 → 上游仍收到 kimi-k3
```

上游模式必须含恰好一个 `*`（它决定捕获哪一段），对外名模式用捕获值填充；两个模式都不收 `?`。
`allow` / `deny` 先按上游 id 判断，改名在它之后：准入描述的是「上游有哪些东西」，暴露叫什么名字
是另一件事。改名是替换——原名不再对客户端存在；两条清单项改名后撞同一个对外名会报错。

不依赖清单的精确改名仍用 `model <对外名> <上游名>`：上游清单漏项时它同样生效，也支撑发现
失败时的降级。

**显式 `model` 与发现结果的关系**：显式声明的模型永远生效、不参与过滤；与发现项同名时以显式
那条为准，于是可以借它改写上游名。一个端点的可用模型 = 显式声明 ∪ 过滤改名后的发现模型。

**发现失败**时：端点有显式模型就退回它们并记一条 `error`；没有显式模型则装配失败（那类端点在
发现失败后没有任何可路由的模型）。reload 失败时旧配置继续服务，在途请求不打断。发现只在启动与
reload 时各做一次，运行中不自动刷新；清单自述还有下一页时记一条提醒，本版不翻页。

客户端可以直接问网关有哪些模型：

```sh
curl -H "Authorization: Bearer $NOVA_CLIENT_KEY" http://127.0.0.1:8080/v1/models
```

`GET /v1/models` 的形状按请求头区分：带 `anthropic-version` 的回答 Anthropic 形状，其余回答
OpenAI 形状；与其它路径一样要过 `client_key`。想在动手改配置前看清目录，用 `nova models -c Novafile`
（它会连上游）：它按端点列出显式声明的模型（含 `model <对外名> <上游名>` 的别名映射）、
清单里保留与被 `allow` / `deny` 排除的模型 id，再加一行目录汇总；`--provider <名>` 只看一条渠道
（汇总也只算这条渠道），`--verbose` 给保留项附上 `static` / `discovered` 来源标注。

### 选路与回退

客户端请求里的 `model` 只用来选路，**不会照转给上游**：

- 同一个对外名出现在多个端点下，它们就是这个名字下的候选；写了 `route` 块时按规则
  排定顺序与轮转，没写时按声明顺序依次尝试；
- 一条渠道声明了多个账号时，账号在端点内层展开：`E1#1 → E1#2 → E2#1 → E2#2`。
  账号是同一渠道内的等价副本，账号级故障比端点级故障常见，内层因此能少打一次
  已知无效的账号；写了 `balance` 时只改每组账号的起点；
- 同一个端点内重复声明同一个对外名是配置错误——那两条候选的差别无从确定；
  发现来的模型与显式声明同名不算重复：显式那条胜出，用于改写上游名；
- 命中不了任何对外名时返回 `model_not_found`，网关不会猜一条渠道转发。
  被 `allow` / `deny` 过滤掉的模型与上游不存在的模型对客户端不可区分。

### 协议转换

客户端协议由请求路径决定（`/v1/chat/completions`、`/v1/responses`、`/v1/messages`，
以及 Gemini 的 `/v1beta/models/<模型>:generateContent` 与 `:streamGenerateContent`），
与端点协议不一致时由网关转换，配置里不需要声明。Gemini 客户端路径里的模型名取 `models/`
前缀之后的部分，与另外三种协议一样按对外名匹配选路。

### 客户端鉴权

`client_key` 列出允许调用网关的凭据，可写多条。客户端用 `Authorization: Bearer <key>`、
`x-api-key: <key>` 或 `x-goog-api-key: <key>`（Gemini 客户端）提交，任一匹配即放行。
不写 `client_key` 时不鉴权，并在 `listen` 绑到非回环地址时记一条警告。

### 日志

`log_level` 决定**哪些记录被输出**，`log_format` 决定**记录长什么样**。两者都是配置指令，
不留一半在环境变量里——否则「这个 nova 会输出什么」就有了两个来源。

text 模式的排版单位是**一次请求一个块**：`access` 主行在前，同一请求的上游明细在后。
块内所有行共用主行的时刻，字段从同一列开始，块与块之间靠关联键前缀分开。

- **access**：入口层看到的事实（客户端协议、请求的模型名、HTTP 状态、耗时、客户端地址）。
  唯一一次成功的上游尝试会被折进这一行，用量与上游 id 因此也在主行上。
- **upstream**：上游那一侧的事实（打到哪条渠道、两侧协议与模型名、上游用量、上游报文）。
  一次请求可能有多条：候选回退时每条候选各一条，`#1` `#2` 就是回退顺序。
  渠道声明了多个账号时带一节 `acct #2`，单账号时不出现。
  主行已经承载的结论（客户端状态码、耗时、错误码）不在这里重抄。
- **catalog**：启动与 reload 时的模型发现结果。行上直接列出客户端可用的对外名
  （`expose` 改名后的名字），超过 20 个时截断并给出总数；json 模式下 `models` 字段给全量。

常见的单次成功请求只占一行，真正有增量的请求才多出行来：

```
13:06:30 INFO   access    c3ccfe…  openai_chat  gpt-test  stream  200  1ms  41/128 tok  via chatmock 127.0.0.1:18080  127.0.0.1:33012
13:06:31 ERROR  access    f7ba49…  openai_chat  gpt-fail  502  42ms  upstream_unavailable  127.0.0.1:33014
13:06:31 ERROR  upstream  f7ba49…  #1  chatmock 127.0.0.1:18080  gpt-fail→fail-model  上游 HTTP 状态码 503：{"error": {"message": "upstream is having a bad day"}}
13:06:32 INFO   access    1d0c9a…  openai_chat  gpt-test  200  8.1s  tries 2  127.0.0.1:33016
13:06:32 WARN   upstream  1d0c9a…  #1  chatmock 127.0.0.1:18080  failed  4.0s  upstream_timeout
13:06:32 INFO   upstream  1d0c9a…  #2  chatmock 127.0.0.1:18080  ok  4.1s  41/128 tok
```

级别：主行按 HTTP 状态分档（5xx 记 `error`、4xx 记 `warn`、其余 `info`）；明细取自身级别与
主行级别中的较高者，唯一例外是客户端取消——它记 `debug`，主行不会把它抬起来。
取消之所以记 debug，是因为长流场景下客户端主动断开很常见，记成 warn 会把真正的上游故障淹没。

同一类上游失败（同一条渠道上的同一个错误码）在 10 秒窗口内只留首条全文，其余折叠计数，
窗口结束时补一条 `×N` 汇总行：上游持续限流时逐字相同的报文会连刷几十行，
真正的信号「上游一直在限流」反而被淹没。

启动横幅不受级别过滤：它回答的是「这个进程在用哪份配置跑」。

**两种格式的分工是「给人看」与「给机器看」**，不是同一条记录的两种排版：text 会省略
「常见情况下不提供信息」的字段（同协议不写协议、同模型不写模型、用量只写非零子项、
单次成功尝试不单独成行、关联键只留前 8 个字符），因此它**不可逆，不能拿来当数据源**。
要解析就切 `log_format json`：那里字段齐全，每个请求按 `request_id` 关联的 `access` 与
`upstream_attempt` 两条记录都在，用量收在 `usage` 对象里，时间戳是 RFC3339，也不做折叠。

能不能显示颜色**不是配置项**：它是环境事实，不是使用者的意图。输出落在终端上、
未设 `NO_COLOR`、且 `TERM` 不是 `dumb` 时才给级别与错误码着色。

### 环境变量与拆分文件

取值里的 `{env.NAME}` 在加载时替换为环境变量 `NAME` 的取值；凭据不要明文写进配置文件。

`import <glob>` 把另一个文件的内容原地拼进来，相对路径相对于写下这一行的文件。
一条 import 至多一个通配符（`*` 或 `?`），不命中任何文件只警告，不阻止加载。

### 配置语法代数

`version` 声明这份配置按哪一代语法书写。nova 程序自身的版本用 `x.y.z`：

- 兼容性变更 → `z+1`
- 不兼容的语法变更 → `y+1`、`z=0`，并把配置语法代数 `+1`
- 大功能变更 → `x+1`、`y=0`、`z=0`

二进制声明自己支持的代数区间。配置声明的代数落在区间之外时，加载会**在解析任何
指令之前**失败，报出「这份配置声明了什么、本二进制支持什么」，而不是先撞上一句
「未知指令」——后者会让人误以为是自己写错了词，而真相是版本对不上。

配置省略 `version` 时按本二进制实现的最高代数解析，并记一条提示。

## 命令行

```
nova run [-c PATH]              启动网关
nova reload [-c PATH]           让运行中的网关重新加载配置
nova config check [-c PATH]     只校验配置，不启动
nova models [-c PATH]           列出网关会认哪些模型（会连上游）
nova version [--json]           打印版本信息
nova help [COMMAND]             帮助
```

全局标志：`-h` / `--help` / `-?`、`-v` / `--version`。

退出码：`0` 成功、`1` 运行期失败、`2` 用法错误。

### 配置校验

`nova config check` 只读配置、不启动服务，适合放进 CI 或提交前钩子：

```sh
nova config check -c Novafile || exit 1
```

配置里有发现型端点（写了 `discover`）时，它额外说明「清单内容由上游决定、本次校验没有连上游」：
不联网是这条命令的定位，而清单里的模型是否真的可用，只有 `nova models` 或启动日志能回答。

### 热重载

`nova run` 会在配置里的 `admin` 地址上多监听一个管理端点，`nova reload` 按它把
「用这份配置重新装配」投递过去。监听地址不变、在途请求不打断；配置写坏时旧配置
原样继续服务，命令以非 0 退出并报出 `文件:行:列`。

改 `listen` 或 `admin` 本身需要重启，reload 会明确拒绝而不是静默沿用旧值。

systemd 下的典型接法：

```ini
[Service]
ExecStart=/usr/local/bin/nova run -c /etc/nova/Novafile
ExecReload=/usr/local/bin/nova reload -c /etc/nova/Novafile
```

## 版本

```sh
nova version
nova version --json          # 给脚本消费：程序版本 + 支持的配置代数 + 指令清单
```

版本号由 `make build-binary` 注入，取值顺序为 `VERSION=` > **最近的 git tag**（工作区有
未提交改动时带 `-dirty`）> `0.0.0`。二进制内嵌的 VCS 信息会单独报出「构建自哪个提交」，
两件事不合并成一个字段：合并后的 `v0.0.0-2-g4700f09` 既不能当版本号比较，也不是提交号。

## 开发

```sh
make check        # 编译 + vet + 单测(-race) + 格式检查
make test
make build-binary
```

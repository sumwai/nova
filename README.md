# nova

大模型 API 转发网关。一个二进制、一份配置文件、几条命令。

## 当前状态

程序本体与配置层已经可用：命令行、Novafile 解析与校验、版本查询、配置热重载。

**转发链路尚未实现**：Novafile 已经能声明上游渠道与模型映射（`provider` / `endpoint` /
`model`），但装配层还没有把它们接上。因此 `nova run` 起来后只提供 `/healthz` 存活探针，
其余路径一律返回 501。用 `nova config check` 可以先把配置写对。

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
`log_level`（缺省 `info`）、`client_key`（可写多条；不写则不鉴权，并在绑非回环时告警）、
`import`（把另一个文件拼进来）。

### 上游渠道

`provider <名>` 描述一条上游渠道。名字只用于日志与错误定位，不参与选路。
凭据 `api_key` 写在 provider 一级，本块的**所有端点共用这一份**。

一条端点由三件事构成：**地址、协议、它提供哪些模型**。

| 指令 | 位置 | 必填 | 说明 |
|---|---|---|---|
| `url` | 端点 | 是 | 完整地址，含协议段与端点路径 |
| `protocol` | 端点 | 否 | `openai_chat` / `openai_responses` / `anthropic_messages`；省略时从 `url` 末段推导，写了则必须与推导一致 |
| `timeout` | 端点 | 否 | 单次上游调用超时，缺省 `60s` |
| `model <对外名> [<上游名>]` | 端点 | 至少一条 | 对外名是客户端请求里要匹配的名字；省略上游名时，发往上游的名字与对外名相同 |

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

### 选路与回退

客户端请求里的 `model` 只用来选路，**不会照转给上游**：

- 同一个对外名出现在多个端点下，它们就是这个名字下的候选，按声明顺序依次尝试；
- 同一个端点内重复声明同一个对外名是配置错误——那两条候选的差别无从确定；
- 命中不了任何对外名时返回 `model_not_found`，网关不会猜一条渠道转发。

### 协议转换

客户端协议由请求路径决定（`/v1/chat/completions`、`/v1/responses`、`/v1/messages`），
与端点协议不一致时由网关转换，配置里不需要声明。

### 客户端鉴权

`client_key` 列出允许调用网关的凭据，可写多条。客户端用 `Authorization: Bearer <key>`
或 `x-api-key: <key>` 提交，任一匹配即放行。不写 `client_key` 时不鉴权，并在 `listen`
绑到非回环地址时记一条警告。

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

### 热重载

`nova run` 会在配置里的 `admin` 地址上多监听一个管理端点，`nova reload` 按它把
「用这份配置重新装配」投递过去。监听地址不变、在途请求不打断；配置写坏时旧配置
原样继续服务，命令以非 0 退出并把 `文件:行:列` 交回给你。

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

版本号由 `make build-binary` 注入，取值顺序为 `VERSION=` > `git describe --tags --dirty`
> `0.0.0`。二进制内嵌的 VCS 信息会单独报出「构建自哪个提交」。

## 开发

```sh
make check        # 编译 + vet + 单测(-race) + 格式检查
make test
make build-binary
```

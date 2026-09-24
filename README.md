# nova

大模型 API 转发网关。一个二进制、一份配置文件、几条命令。

## 当前状态

第一版只实现程序本体：命令行、配置加载与校验、版本查询、配置热重载。

转发链路（上游渠道、协议适配、流式转发）**尚未实现**：`nova run` 起来后只提供
`/healthz` 存活探针，其余路径一律返回 501 并说明转发尚未实现。

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

Novafile 的语法是：全局指令直接写在顶层、一行一条。

```
version 1
log_level info
listen :8080
admin localhost:2026
```

完整注释见 [`Novafile.example`](Novafile.example)。

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

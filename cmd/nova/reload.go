package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/gateway"
)

// reloadClientTimeout 是重载请求的上限。
//
// 取得比一次普通 HTTP 请求长得多：服务端要重新装配整份配置，而调用方在这里
// 超时并不能取消服务端已经开始的装配工作，只会让「到底成没成」变得不确定。
const reloadClientTimeout = 5 * time.Minute

// reloadErrorLimit 是读回失败原因的字节上限。
//
// 失败原因是一句话，不是一整份日志。设上限是为了让一个坏掉的服务端不能靠
// 一段超长响应把调用方的内存吃光。
const reloadErrorLimit = 64 << 10

func newReloadCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "reload",
		Short: "让运行中的网关重新加载配置",
		Long: `按配置里的 admin 地址找到运行中的网关，把「用这份配置重新装配」投递过去。

监听地址不变、在途请求不打断；配置写坏时旧配置原样继续服务，本命令以非 0 退出
并把 文件:行:列 交回。要改 listen 或 admin 本身请重启网关。`,
		Args: rejectExtraArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := resolveConfigPath(configPath, os.Getenv)
			if err != nil {
				return err
			}
			return requestReload(cmd.Context(), path, cmd.OutOrStdout())
		},
	}
	registerConfigFlag(cmd, &configPath)
	return cmd
}

// requestReload 把重载请求投递给运行中的网关。
//
// admin 地址从本地读同一份配置得到，因此本命令与服务必须在同样的环境变量下
// 运行、并读到同一份文件。这个前提写进错误消息里：它是「reload 连不上」时
// 唯一需要检查的东西，写出来能省掉一轮「哪一步不对」的排查。
func requestReload(ctx context.Context, path string, stdout io.Writer) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}

	endpoint := "http://" + dialAddress(cfg.Admin) + gateway.AdminPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(path))
	if err != nil {
		return fmt.Errorf("构造重载请求失败：%w", err)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	client := &http.Client{Timeout: reloadClientTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("连不上管理端点 %s（%w）；网关在跑吗？它读的是同一份配置吗？", cfg.Admin, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, reloadErrorLimit))
	if resp.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			// 服务端没给出原因时退回状态行本身：比一句「重载失败」多出
			// 一个状态码，至少能指向 404（路径不对）与 400（请求体不对）。
			detail = resp.Status
		}
		return fmt.Errorf("网关拒绝了这次重载：%s", detail)
	}

	_, _ = fmt.Fprintf(stdout, "已重载 %s（管理端点 %s）\n", path, cfg.Admin)
	return nil
}

// dialAddress 把监听地址转成可拨号的地址。
//
// 监听地址里的空 host 与 0.0.0.0 表示「绑所有接口」，它们不是可拨号的目标：
// 直接拼进 URL 会得到 http://:2026（没有主机）或 http://0.0.0.0:2026
// （在多数系统上连不通）。统一换成回环地址，语义正是「连本机这个端口」。
func dialAddress(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

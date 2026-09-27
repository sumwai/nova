package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/gateway"
	"github.com/sumwai/nova/internal/probe"
	"github.com/sumwai/nova/internal/profile"
)

// dataEnvVar 是 XDG 数据目录的环境变量名。探测命令只允许放在它下面的 nova/probes/。
const dataEnvVar = "XDG_DATA_HOME"

func newLimitsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "limits",
		Short: "额度探测相关操作",
		Args:  rejectExtraArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newLimitsCheckCmd(), newLimitsClearCmd())
	return cmd
}

func newLimitsCheckCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "check <provider>",
		Short: "跑一次渠道声明的 exec 探测并回显解析出的额度文档",
		Long: `读取并展开配置，找到指定渠道引用的档案，跑一次它声明的 exec 用量探测，把解析出的
额度文档原样回显：来源、观测时刻、到期时刻，以及账号级与模型级的各条 kind·metric·window
与 limit / remaining / used / resets_at。

exec 命令必须是 ${XDG_DATA_HOME:-~/.local/share}/nova/probes/ 下的绝对路径，组或其他人
不可写；命令以 argv 数组直接执行，不经 shell，环境变量清空，超时后强杀。

内置 probe（usage.probe）本版未实现：各平台的用量接口是需要实测确认的平台事实，
先在档案里写 exec 指向本地脚本。

本命令只读探测输出，不写额度快照、不启动网关。`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(configPath, os.Getenv)
			if err != nil {
				return err
			}
			cfg, err := loadConfig(path)
			if err != nil {
				return err
			}
			provider, err := findProvider(cfg, args[0])
			if err != nil {
				return err
			}
			if provider.Usage == nil {
				return fmt.Errorf("provider %s 引用的档案没有声明 usage 探测；"+
					"在档案里写 usage { exec <绝对路径> } 后再试", provider.Name)
			}
			if provider.Usage.Exec == "" {
				return fmt.Errorf("provider %s 的档案声明的是内置 probe %q；本版未实现内置探测，"+
					"请改用 usage { exec <绝对路径> }", provider.Name, provider.Usage.Probe)
			}
			dataDir, err := defaultDataDir()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			runner := &probe.Runner{DataDir: dataDir}
			doc, err := runner.Run(ctx, provider.Usage.Exec)
			if err != nil {
				return err
			}
			printLimitsDoc(cmd.OutOrStdout(), provider.Name, doc)
			return nil
		},
	}
	registerConfigFlag(cmd, &configPath)
	return cmd
}

// defaultDataDir 返回 nova 的数据目录（${XDG_DATA_HOME:-~/.local/share}/nova）。
//
// 与状态目录、配置目录同一口径：环境变量优先，退回 XDG 缺省路径。
func defaultDataDir() (string, error) {
	if dir := os.Getenv(dataEnvVar); dir != "" {
		return filepath.Join(dir, "nova"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("无法确定数据目录（%w）", err)
	}
	return filepath.Join(home, ".local", "share", "nova"), nil
}

// findProvider 按名字在展开后的配置里找一条渠道。
func findProvider(cfg *config.Config, name string) (*config.Provider, error) {
	names := make([]string, 0, len(cfg.Providers))
	for i := range cfg.Providers {
		if cfg.Providers[i].Name == name {
			return &cfg.Providers[i], nil
		}
		names = append(names, cfg.Providers[i].Name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("配置里没有任何渠道，无法检查 %q", name)
	}
	return nil, fmt.Errorf("配置里没有渠道 %q；已声明的渠道：%s", name, strings.Join(names, "、"))
}

// printLimitsDoc 把一份额度文档排成人读清单。
func printLimitsDoc(w io.Writer, provider string, doc *profile.LimitsDoc) {
	_, _ = fmt.Fprintf(w, "provider %s 的探测结果\n", provider)
	_, _ = fmt.Fprintf(w, "  source      %s\n", orNone(doc.Source))
	_, _ = fmt.Fprintf(w, "  observed_at %s\n", orNone(doc.ObservedAt))
	if doc.ExpiresAt != "" {
		_, _ = fmt.Fprintf(w, "  expires_at  %s\n", doc.ExpiresAt)
	}

	if len(doc.Account) == 0 && len(doc.Models) == 0 {
		_, _ = fmt.Fprintln(w, "  （文档没有任何额度条目）")
		return
	}
	if len(doc.Account) > 0 {
		_, _ = fmt.Fprintln(w, "  账号级（跨模型共享）")
		for _, limit := range doc.Account {
			_, _ = fmt.Fprintf(w, "    %s\n", describeLimit(limit))
		}
	}
	for _, model := range sortedLimitModels(doc.Models) {
		_, _ = fmt.Fprintf(w, "  模型 %s\n", model)
		for _, limit := range doc.Models[model] {
			_, _ = fmt.Fprintf(w, "    %s\n", describeLimit(limit))
		}
	}
}

// describeLimit 把一条额度条目排成一行。
func describeLimit(limit profile.Limit) string {
	parts := []string{limit.Kind, limit.Metric + "/" + limit.Window}
	if limit.Limit != nil {
		parts = append(parts, "limit="+formatAmount(*limit.Limit))
	}
	if limit.Remaining != nil {
		parts = append(parts, "remaining="+formatAmount(*limit.Remaining))
	}
	if limit.Used != nil {
		parts = append(parts, "used="+formatAmount(*limit.Used))
	}
	if limit.ResetsAt != "" {
		parts = append(parts, "resets_at="+limit.ResetsAt)
	}
	if limit.Pool != "" {
		parts = append(parts, "pool="+limit.Pool)
	}
	if limit.Unbounded {
		parts = append(parts, "unbounded")
	}
	if limit.Assumed {
		parts = append(parts, "assumed")
	}
	return strings.Join(parts, "  ")
}

// formatAmount 把额度数值排成不带多余零的十进制写法。
//
// 用 'f' 而不是 'g'：token 这类大整数在 'g' 下会变成 2e+06，回显时反而不好读。
func formatAmount(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// sortedLimitModels 按字典序返回模型名，让回显顺序稳定。
func sortedLimitModels(models map[string][]profile.Limit) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func newLimitsClearCmd() *cobra.Command {
	var configPath string
	var model string
	cmd := &cobra.Command{
		Use:   "clear <provider>",
		Short: "清除运行中网关的额度标记与静态 absolute 判定",
		Long: `按配置里的 admin 地址找到运行中的网关，投递一次显式清除：

- 清掉该渠道账号上学习到的不可用标记与连续失败计数；
- 撤销由档案静态声明推算出的 absolute 判定，让「已耗尽」的账号重新参与选路。

它服务于 window: absolute 且 remaining: 0 这类没有自动恢复点的条目；
其他窗口会在窗口切换时自行恢复，不需要这个入口。清除是显式动作，会让一个
其实已耗尽的账号重新被选中，因此只在明确知道余额已恢复时使用。

不带 --model 时清账号级判定；带上时额外限定到该模型的模型级条目。`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(configPath, os.Getenv)
			if err != nil {
				return err
			}
			return requestLimitsClear(cmd.Context(), path, args[0], model, cmd.OutOrStdout())
		},
	}
	registerConfigFlag(cmd, &configPath)
	cmd.Flags().StringVar(&model, "model", "", "额外限定到该模型的模型级条目")
	return cmd
}

// requestLimitsClear 把显式清除请求投递给运行中的网关。
func requestLimitsClear(ctx context.Context, path, provider, model string, stdout io.Writer) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(map[string]string{"provider": provider, "model": model})
	if err != nil {
		return fmt.Errorf("构造清除请求失败：%w", err)
	}
	endpoint := "http://" + dialAddress(cfg.Admin) + gateway.LimitsClearPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("构造清除请求失败：%w", err)
	}
	req.Header.Set("Content-Type", "application/json")

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
			detail = resp.Status
		}
		return fmt.Errorf("网关拒绝了这次清除：%s", detail)
	}

	var result struct {
		Cleared int `json:"cleared"`
	}
	_ = json.Unmarshal(body, &result)
	_, _ = fmt.Fprintf(stdout, "已清除渠道 %s 的额度标记（管理端点 %s，涉及 %d 个账号）\n",
		provider, cfg.Admin, result.Cleared)
	return nil
}

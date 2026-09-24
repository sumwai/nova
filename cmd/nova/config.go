package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "配置相关操作",
		Args:  rejectExtraArgs,
		// 只给了子命令名而不给子命令时打帮助：使用者多半是在找下一步该敲什么，
		// 报一句「缺少子命令」既不告诉他有哪些可选，也多出一个要处理的退出码。
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newConfigCheckCmd())
	return cmd
}

func newConfigCheckCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "check",
		Short: "只校验配置，不启动网关",
		Long: `读取并完整校验一份配置，把结论打成一行。

它不监听任何端口、不连接上游，因此可以放进 CI 或提交前的钩子：
配置写错时以非 0 退出，并给出 文件:行:列。`,
		Args: rejectExtraArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := resolveConfigPath(configPath, os.Getenv)
			if err != nil {
				return err
			}
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}

			// 提醒走 stderr、结论走 stdout：结论单独留在 stdout 上，
			// `nova config check > /dev/null` 之类的调用才不必滤掉提醒行。
			for _, warn := range cfg.Warnings {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "提醒 %s\n", warn.String())
			}
			// 发现型端点的可用模型只能在运行期确定：check 不连上游，因此这件事
			// 必须被说出来，而不是让「校验通过」被误读成「上游那些模型也能用」。
			if count := cfg.DiscoveryCount(); count > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"提醒 %s：%d 条端点声明了模型发现（discover），清单内容由上游决定；本次校验没有连上游，用 nova models 查看实际结果\n",
					cfg.Path, count)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"%s 校验通过（配置代数 %d，监听 %s，管理端点 %s）\n",
				cfg.Path, cfg.Schema, cfg.Listen, cfg.Admin)
			return nil
		},
	}
	registerConfigFlag(cmd, &configPath)
	return cmd
}

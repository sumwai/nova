package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/price"
	"github.com/sumwai/nova/internal/profileapply"
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
			cfg, err := loadConfig(path)
			if err != nil {
				return err
			}
			if err := validatePriceFile(cfg); err != nil {
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
			// 账号池摘要回答「同一个渠道里的凭据怎么选」：缺省是声明顺序，
			// 想分摊要写 balance。这一行是首次让使用者知道调度这件事存在的地方。
			if pools, accounts := cfg.AccountPoolStats(); pools > 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"账号池：%d 个渠道共 %d 个账号，%s；写 balance 改为按权重轮询分摊\n",
					pools, accounts, cfg.AccountPoolPolicyText())
			}
			return nil
		},
	}
	registerConfigFlag(cmd, &configPath)
	return cmd
}

// loadConfig 解析并展开一份配置，供不联网的命令行路径使用。
//
// config check 与 models 都要把档案引用展开成具体端点，因此两者共用同一份入口。
// 状态目录与配置目录就在这里读：展开远端源需要前者，展开凭据需要后者，
// 而两者都属于本机状态，不属于配置。
func loadConfig(path string) (*config.Config, error) {
	// 状态目录不可得（HOME / XDG_STATE_HOME 都缺失）时退回空串：展开层把它当作
	// 「没有可用快照」，只有配置真引用到远端源的档案时才会在错误消息里说明原因。
	// config check 与 models 在引入档案引用之前不依赖主目录，不该因它失败。
	stateDir, err := defaultStateDir()
	if err != nil {
		stateDir = ""
	}
	// 配置目录同理：定位不到时不读凭据库，只有真引用到档案的渠道才会在展开期报错。
	configDir, err := defaultConfigBaseDir()
	if err != nil {
		configDir = ""
	}
	return profileapply.Load(path, profileapply.Options{
		StateDir:  stateDir,
		ConfigDir: configDir,
		Getenv:    os.Getenv,
	})
}

// validatePriceFile 在不联网的校验路径上读取 prices 块声明的价格文件。
//
// 装配期本就要求这份文件可读（读不到即装配失败），但不联网的 config check 若不读它，
// 路径写错只能等到 nova run 才发现——而那正是「校验通过」最容易被误读的场景。
// 这里只做「能打开并解析」这一层，价格数字如何参与排序仍由装配期决定。
func validatePriceFile(cfg *config.Config) error {
	if !cfg.Prices.PathDeclared {
		return nil
	}
	if _, err := price.LoadFile(cfg.Prices.Path); err != nil {
		return config.PriceFileError(cfg.Prices, err)
	}
	return nil
}

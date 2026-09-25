package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/gateway"
)

// configEnvVar 是通过环境变量指定配置文件路径时使用的变量名。
//
// 取 NOVA_CONFIG 而不是 NOVAFILE：它指向的是一份可以被放在任何位置、
// 任何名字的配置，用文件名当变量名会暗示「它只认 Novafile 这个文件名」。
const configEnvVar = "NOVA_CONFIG"

// stateEnvVar 是 XDG 状态目录的环境变量名。
//
// Go 标准库只提供 UserConfigDir 与 UserCacheDir，没有 UserStateDir，因此这一项要自己读。
const stateEnvVar = "XDG_STATE_HOME"

func newRunCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "启动网关",
		Long: `启动网关：装配配置、监听数据面与管理端点，直到收到 INT 或 TERM。

配置路径的取值顺序是 命令行 -c > 环境变量 NOVA_CONFIG > XDG 缺省路径。`,
		Args: rejectExtraArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := resolveConfigPath(configPath, os.Getenv)
			if err != nil {
				return err
			}
			statePath, err := defaultStatePath()
			if err != nil {
				return err
			}
			ctx, stop := notifyShutdown(cmd)
			defer stop()

			return gateway.Run(ctx, gateway.Options{
				ConfigPath: path,
				StatePath:  statePath,
				LogOutput:  cmd.ErrOrStderr(),
			})
		},
	}
	registerConfigFlag(cmd, &configPath)
	return cmd
}

// notifyShutdown 把 INT 与 TERM 转成 ctx 的取消，并就地报出收到的是哪个信号。
//
// 不用 signal.NotifyContext：它取消 ctx 时会丢掉信号身份，于是日志里只会留下
// 一句「context canceled」，而「是被 INT 打断还是被 systemd 的 TERM 停下」
// 正是排查「服务为什么退出」时首先要分清的一件事。
//
// 收尾（停监听、等在途请求）由 gateway.Run 负责，这里只管把「该停了」传下去。
func notifyShutdown(cmd *cobra.Command) (context.Context, func()) {
	ctx, cancel := context.WithCancel(cmd.Context())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	go func() {
		select {
		case sig := <-signals:
			fmt.Fprintf(cmd.ErrOrStderr(), "收到信号 %s，正在停止\n", sig)
			cancel()
		case <-ctx.Done():
		}
	}()

	return ctx, func() {
		signal.Stop(signals)
		cancel()
	}
}

// registerConfigFlag 注册配置文件路径这一对短长写法。
//
// 两个名字写进同一个变量，因此它们不是两份取值：同一件事只有一个落点。
// 命令行上两个都写时按 pflag 的惯例最后一个生效，不另设优先级规则——
// 给一个本该被避免的输入发明语义，只会多出一条要记住的规则。
func registerConfigFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVarP(target, longConfig, shortConfig, "",
		"配置文件路径（缺省取 $"+configEnvVar+"，再退回 XDG 配置目录下的 nova/Novafile）")
}

// resolveConfigPath 按「命令行 > 环境变量 > 缺省路径」敲定配置文件路径。
//
// getenv 作为入参注入而不是直接调用 os.Getenv：这条顺序因此不依赖进程环境
// 就能被检查，而「取值来源的优先级」恰恰是只靠读代码很难确认的一件事。
func resolveConfigPath(flagValue string, getenv func(string) string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if fromEnv := getenv(configEnvVar); fromEnv != "" {
		return fromEnv, nil
	}
	return defaultConfigPath()
}

// defaultStatePath 返回统计库的缺省路径：状态目录下的 nova/stats.db。
//
// 状态目录与配置目录分开：配置回答「希望怎么跑」，状态记录「实际跑过什么」，
// 前者通常纳入版本控制或由运维下发，后者是本机数据，混在一起会让两者互相牵连。
//
// 定位不到主目录时报错而不是退回当前目录：相对路径会让「上次的统计被写到哪了」
// 随工作目录漂移，而那是重启后对不上账时最难查的一件事。
func defaultStatePath() (string, error) {
	if dir := os.Getenv(stateEnvVar); dir != "" {
		return filepath.Join(dir, "nova", "stats.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("无法确定状态目录（%w）；统计库没有落点", err)
	}
	return filepath.Join(home, ".local", "state", "nova", "stats.db"), nil
}

// defaultConfigPath 返回 XDG 约定下的缺省配置路径。
//
// 定位不到用户配置目录时报错，而不是退回当前目录的相对路径：相对路径会让
// 「nova 到底读了哪份配置」随工作目录漂移，而这是排障时最不该有歧义的一件事。
func defaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法确定用户配置目录（%w）；请用 -c 指定配置文件路径", err)
	}
	return filepath.Join(dir, "nova", "Novafile"), nil
}

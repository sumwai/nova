package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// 标志名。短名与长名都写成常量，让「谁占了 -v」这件事只有一处可查：
// 短名是命令行上最稀缺的资源，散落在字面量里迟早撞车。
const (
	longConfig   = "config"
	longVersion  = "version"
	longJSON     = "json"
	shortConfig  = "c"
	shortVersion = "v"
)

// newRootCmd 组装整棵命令树。
//
// 每次调用都造一棵新的树，而不是复用包级单例：cobra 的命令对象带状态
// （解析过的标志、设置过的输出），复用会让多次调用互相污染，
// 而测试恰恰就是「在同一个进程里多次调用」。
func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "nova",
		Short: "大模型 API 转发网关",
		Long: `nova 是一个大模型 API 转发网关。

它用一份 Novafile 描述「监听哪里、管理端点在哪」，并把它装配成运行中的服务。
配置改动通过 nova reload 投递给运行中的进程，不需要重启。`,
		// 用法与错误由本文件与 main.go 统一输出，因此关掉 cobra 自带的那两次打印：
		// 打开时它会同时打错误和整段用法，而输出里不含任何退出码信息。
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          rejectExtraArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ask, _ := cmd.Flags().GetBool(longVersion); ask {
				return printVersion(cmd.OutOrStdout(), false)
			}
			// 不带子命令时打帮助而不是报错：`nova` 本身是一个合法调用，
			// 使用者的意图通常是「看看这工具能干什么」。
			return cmd.Help()
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetUsageTemplate(chineseUsageTemplate)

	// pflag 把标志解析错误也当成普通错误返回。这里就地打印错误与用法，
	// 再交回一个只用于选退出码的标记。
	//
	// 用法写 ErrOrStderr 而不是调 c.Usage()：Usage() 走 OutOrStderr，
	// 而根命令已经 SetOut 到结果流，于是用法会被写进 stdout。
	// `nova --bad > result.txt` 因此会把一段用法说明混进结果文件里。
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		fmt.Fprintf(c.ErrOrStderr(), "nova: %v\n\n", err)
		fmt.Fprint(c.ErrOrStderr(), c.UsageString())
		return errUsage
	})
	root.Flags().BoolP(longVersion, shortVersion, false, "打印版本信息并退出")

	root.AddCommand(
		newRunCmd(),
		newReloadCmd(),
		newConfigCmd(),
		newModelsCmd(),
		newVersionCmd(),
	)
	// completion 提前建出来，而不是等 cobra 在 Execute 时自己补：
	// 它挂在 root 上、说明文案交给下面的 localizeBuiltinHelp 统一汉化，
	// 而那时它必须已经是一棵现有子命令树里的成员。
	root.InitDefaultCompletionCmd()
	localizeBuiltinHelp(root)
	return root
}

// localizeBuiltinHelp 把 cobra 自动挂上的内建命令与标志的说明换成中文。
//
// cobra 的默认文案是英文。不统一的话，整棵树的 help 里只有它自己补上的那几条
// 是英文，中文使用者会以为这一版没做完。递归处理是必需的：
// 每个子命令的 -h 也是 cobra 在各自 execute 时才补上的。
func localizeBuiltinHelp(cmd *cobra.Command) {
	cmd.InitDefaultHelpFlag()
	if helpFlag := cmd.Flags().Lookup("help"); helpFlag != nil {
		helpFlag.Usage = "查看帮助"
	}
	cmd.InitDefaultHelpCmd()

	for _, sub := range cmd.Commands() {
		switch sub.Name() {
		case "help":
			sub.Short = "查看任意命令的帮助"
		case "completion":
			sub.Short = "生成 shell 补全脚本"
		}
		localizeBuiltinHelp(sub)
	}
}

// rejectExtraArgs 拒绝多余的位置参数，并把它归到「用法错误」这一类。
//
// 不用 cobra.NoArgs：它返回的是一条普通错误，于是「敲错了子命令名」会落进
// 退出码 1（运行期失败），而它其实是用法问题。这里就地打印提示并返回 errUsage，
// 让退出码与事实对上——脚本据此才知道该改命令行，而不是去重试。
//
// 措辞按命令是否带子命令分开：对 `nova config chekc` 说「未知子命令」，
// 对 `nova version foo` 说「不接受位置参数」。叶子命令没有子命令可敲错，
// 对它说「未知子命令 foo」会让人白白怀疑自己漏了一个子命令的概念。
func rejectExtraArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if cmd.HasAvailableSubCommands() {
		fmt.Fprintf(cmd.ErrOrStderr(), "nova: 未知子命令 %q\n\n", args[0])
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(), "nova: %s 不接受位置参数，多出来的是 %q\n\n",
			cmd.CommandPath(), args[0])
	}
	fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
	return errUsage
}

// chineseUsageTemplate 是 cobra 默认用法模板的中文版。
//
// 只译标签、不动结构：哪些段落会出现、什么时候出现仍由 cobra 的字段驱动，
// 因此加一条子命令或一个标志都会自动出现在这里，不需要同步维护模板。
const chineseUsageTemplate = `用法：{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [命令]{{end}}{{if gt (len .Aliases) 0}}

别名：
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

示例：
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

可用命令：{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

标志：
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

全局标志：
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableSubCommands}}

用 "{{.CommandPath}} [命令] --help" 查看某个命令的详细说明。{{end}}
`

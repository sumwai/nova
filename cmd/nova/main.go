// Package main 是 nova 的命令行入口。
//
// 命令树由 cobra 组装，但退出码不由它决定：cobra 对所有错误一视同仁，
// 而命令行工具需要把「命令写错了」与「命令失败了」分开，脚本才能据此
// 决定是改脚本还是重试。
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// 进程退出码。
//
// 用法错误取 2 而不是 1：脚本据此能区分「我的命令行不对」（去改脚本，重试无意义）
// 与「命令本身失败了」（可能是环境问题，值得重试或上报）。
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// errUsage 标记「命令行本身不对」这一类错误，只用于在 execute 里选退出码。
//
// 它不携带文本：消息在产生它的地方已经写好并当场打印过，在这里再包一层
// 只会让同一条消息出现两次。
var errUsage = errors.New("用法错误")

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

// execute 跑完一次命令行调用并返回进程退出码。
//
// stdout 与 stderr 作为入参注入而不是就地取 os.Stdout：整棵命令树的输出落点
// 因此能在测试里换成 buffer，「打印了什么」与「退出码是几」都能被断言，
// 而不必去捕获取进程的文件描述符。
func execute(args []string, stdout, stderr io.Writer) int {
	cmd := newRootCmd(stdout, stderr)
	cmd.SetArgs(normalizeArgs(args))

	switch err := cmd.Execute(); {
	case err == nil:
		return exitOK
	case errors.Is(err, errUsage):
		// 用法消息与用法本体已由 SetFlagErrorFunc 就地打印，这里只负责选退出码。
		return exitUsage
	default:
		fmt.Fprintf(stderr, "nova: %v\n", err)
		return exitError
	}
}

// normalizeArgs 把 `-?` 归一成 `--help`。
//
// cobra 不认识 `-?`，而它是流传很广的一种习惯写法。在交给 cobra 之前改写，
// 比注册一个假标志再在 PreRun 里拦截简单得多：后者要和 cobra 内部的
// help 处理顺序配合，而那个顺序是它的实现细节，不是对外承诺。
//
// `--` 之后一律不再改写：那里的 `-?` 是一个位置参数（例如一个真叫 `-?` 的文件），
// 把它变成帮助请求会让一个能用的调用突然失灵。
func normalizeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i, arg := range args {
		if arg == "--" {
			return append(out, args[i:]...)
		}
		if arg == "-?" {
			arg = "--help"
		}
		out = append(out, arg)
	}
	return out
}

package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/credentials"
	"github.com/sumwai/nova/internal/profile"
)

func newLoginCmd() *cobra.Command {
	var (
		keyStdin bool
		keyEnv   string
		noStore  bool
	)
	cmd := &cobra.Command{
		Use:   "login [<档案 id>] [<账号名>]",
		Short: "把一份 API Key 存进凭据库",
		Long: `把一份 API Key 按下「档案 id + 账号名」存进凭据库，供引用该档案的 provider 取用。

省略账号名时用 default。省略档案 id 时列出内置档案的 id 供选择，不联网。
密钥有三种取法：终端上无回显读入（缺省）、--key-stdin 从标准输入读、--key-env VAR 读环境变量；
标准输入不是终端且没有给来源标志时报错，而不是静默读到一个空值。

本版不在登录流程里联网，因此不校验密钥是否有效：一次接受的 401 会推迟到第一次请求。

用 --no-store 只把密钥写到 stdout（供 export 使用），不落盘。`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 2 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"nova: login 至多接受两个位置参数（档案 id 与账号名），多出来的是 %q\n\n", args[2])
				fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
				return errUsage
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if keyStdin && keyEnv != "" {
				return fmt.Errorf("--key-stdin 与 --key-env 只能选一个")
			}
			configDir, err := defaultConfigBaseDir()
			if err != nil {
				return err
			}
			store, err := credentials.Load(configDir)
			if err != nil {
				return err
			}

			if len(args) == 0 {
				return listBuiltinProfiles(cmd)
			}
			profileID := args[0]
			accountName := "default"
			if len(args) == 2 {
				accountName = args[1]
			}

			key, err := readLoginKey(cmd, keyStdin, keyEnv)
			if err != nil {
				return err
			}

			if noStore {
				fmt.Fprintln(cmd.OutOrStdout(), key)
				fmt.Fprintln(cmd.ErrOrStderr(),
					"提醒：--no-store：密钥只写到 stdout，未落盘；未在登录时校验密钥：本版不在登录流程里联网")
				return nil
			}
			if err := store.Set(profileID, accountName, key); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已保存档案 %s 的账号 %s 到 %s\n",
				profileID, accountName, store.Path())
			fmt.Fprintln(cmd.ErrOrStderr(), "提醒：未在登录时校验密钥：本版不在登录流程里联网")
			return nil
		},
	}
	cmd.Flags().BoolVar(&keyStdin, "key-stdin", false, "从标准输入读入密钥")
	cmd.Flags().StringVar(&keyEnv, "key-env", "", "从指定环境变量读入密钥")
	cmd.Flags().BoolVar(&noStore, "no-store", false, "只把密钥写到 stdout，不落盘")
	return cmd
}

// listBuiltinProfiles 列出内置档案的 id，供省略档案 id 的调用选一个。
//
// 只列内置档案：它们随二进制发布，任何机器上都存在，因此这份清单一定可用；
// 订阅源与本地源里的档案要读磁盘或联网，登录流程不碰它们。
func listBuiltinProfiles(cmd *cobra.Command) error {
	builtin, err := profile.Builtin()
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "内置档案：")
	for _, loaded := range builtin {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s\n", loaded.ID, loaded.Name)
	}
	return fmt.Errorf("未给出档案 id；从上面选一个再跑，形如 `nova login <档案 id>`")
}

// readLoginKey 按标志选择密钥的取值来源。
//
// 三种来源互斥且都有明确的失败：两条来源标志同时给出时报错，环境变量为空时报错，
// 标准输入不是终端且没有来源标志时报错并说明可用的标志。不做任何静默兜底——
// 静默取到一个空密钥，会把问题推迟成上游的一次 401。
func readLoginKey(cmd *cobra.Command, keyStdin bool, keyEnv string) (string, error) {
	switch {
	case keyEnv != "":
		value := os.Getenv(keyEnv)
		if value == "" {
			return "", fmt.Errorf("环境变量 %s 未设置或为空", keyEnv)
		}
		return value, nil
	case keyStdin:
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("读取标准输入失败：%v", err)
		}
		key := strings.TrimSpace(string(data))
		if key == "" {
			return "", fmt.Errorf("标准输入里没有读到密钥")
		}
		return key, nil
	}

	input := cmd.InOrStdin()
	file, ok := input.(*os.File)
	if !ok || !isTerminal(file) {
		return "", fmt.Errorf(
			"标准输入不是终端，无法无回显读入密钥；请用 --key-stdin 或 --key-env VAR")
	}
	fmt.Fprint(cmd.ErrOrStderr(), "密钥（不回显）：")
	key, err := readSecretNoEcho(file)
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("没有读到密钥")
	}
	return key, nil
}

// isTerminal 报告一个文件是不是终端字符设备。
//
// 只看设备类型，不看 TERM：TERM 为空只说明终端类型未知，不代表输入不是终端。
func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

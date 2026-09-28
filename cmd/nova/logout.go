package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/keystore"
)

func newLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout [<provider>] [<账号名>]",
		Short: "从凭据库删除账号",
		Long: `从凭据库删除一个账号，并说明删掉了哪一个。

省略参数时列出凭据库里已有的账号（供应商名、账号名与密钥尾四位）供选择；
给 provider 而不给账号名时删该供应商下的 ` + keystore.DefaultAccount + ` 账号。

只改凭据库，不动任何配置文件：删掉一份凭据不影响配置里的 account 声明，
下一次装配会以「账号不在凭据库里」报出，并给出重新登录的命令。`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 2 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"nova: logout 至多接受两个位置参数（provider 名与账号名），多出来的是 %q\n\n", args[2])
				fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
				return errUsage
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := loadKeystore()
			if err != nil {
				return err
			}

			if len(args) == 0 {
				return logoutInteractive(cmd, store)
			}

			namespace := args[0]
			account := keystore.DefaultAccount
			if len(args) == 2 {
				account = args[1]
			}
			if _, ok := store.Lookup(namespace, account); !ok {
				available := store.Names(namespace)
				if len(available) == 0 {
					return fmt.Errorf("凭据库里没有 %s 的账号", namespace)
				}
				return fmt.Errorf("凭据库里没有 %s 的账号 %q；有的账号：%s",
					namespace, account, strings.Join(available, "、"))
			}
			if err := store.Remove(namespace, account); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已删除 %s 的账号 %s\n", namespace, account)
			return nil
		},
	}
	return cmd
}

// logoutInteractive 列出凭据库里的账号，让使用者选一个删除。
//
// 列表带密钥尾四位：同一个供应商下可能有多个账号，只给名字时无法分辨哪个还在用。
func logoutInteractive(cmd *cobra.Command, store *keystore.Store) error {
	entries := store.All()
	if len(entries) == 0 {
		return fmt.Errorf("凭据库里没有账号；可跑 `nova login` 添加")
	}
	choices := make([]choice, 0, len(entries))
	for _, entry := range entries {
		choices = append(choices, choice{
			label: fmt.Sprintf("%s  %s  %s",
				entry.Namespace, entry.Name, keystore.Mask(entry.Key)),
			value: entry.Namespace + "/" + entry.Name,
		})
	}
	index, err := promptChoice(cmd, "请选择要删除的账号", choices)
	if err != nil {
		return err
	}
	target := entries[index]
	if err := store.Remove(target.Namespace, target.Name); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "已删除 %s 的账号 %s\n", target.Namespace, target.Name)
	return nil
}

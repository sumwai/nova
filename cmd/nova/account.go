package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/credentials"
)

func newAccountCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "account",
		Short: "查看与删除凭据库里的账号",
		Args:  rejectExtraArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newAccountListCmd(), newAccountRemoveCmd())
	return cmd
}

func newAccountListCmd() *cobra.Command {
	var show bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出凭据库里的账号",
		Long: `逐行列出凭据库里的账号：档案 id、账号名、加入时间与密钥尾四位。

缺省不回显完整密钥，只给尾四位；--show 才打印完整密钥。
本命令只读凭据库，不读配置、不联网。`,
		Args: rejectExtraArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := loadCredentialStore()
			if err != nil {
				return err
			}
			ids := store.ProfileIDs()
			if len(ids) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(),
					"凭据库里没有账号；可跑 `nova login <档案 id>` 添加")
				return nil
			}
			for _, id := range ids {
				for _, entry := range store.Entries(id) {
					key := credentials.Suffix(entry.Key)
					if show {
						key = entry.Key
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %s  %s\n",
						id, entry.Name, formatAddedAt(entry.AddedAt), key)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&show, "show", false, "打印完整密钥（缺省只显示尾四位）")
	return cmd
}

func newAccountRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <档案 id> [<账号名>]",
		Short: "删除凭据库里的账号",
		Long: `删除一个账号；省略账号名时删除该档案下的全部账号，并在输出里说明删了几个。

只改凭据库，不动任何配置文件。`,
		Args: func(cmd *cobra.Command, args []string) error {
			switch {
			case len(args) == 0:
				fmt.Fprint(cmd.ErrOrStderr(), "nova: remove 需要一个档案 id\n\n")
			case len(args) > 2:
				fmt.Fprintf(cmd.ErrOrStderr(),
					"nova: remove 至多接受两个位置参数（档案 id 与账号名），多出来的是 %q\n\n", args[2])
			default:
				return nil
			}
			fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
			return errUsage
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := loadCredentialStore()
			if err != nil {
				return err
			}
			profileID := args[0]

			if len(args) == 1 {
				count := len(store.Names(profileID))
				if count == 0 {
					return fmt.Errorf("凭据库里没有档案 %s 的账号", profileID)
				}
				if err := store.Remove(profileID, ""); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "已删除档案 %s 的 %d 个账号\n", profileID, count)
				return nil
			}

			name := args[1]
			if _, ok := store.Lookup(profileID, name); !ok {
				available := store.Names(profileID)
				if len(available) == 0 {
					return fmt.Errorf("凭据库里没有档案 %s 的账号", profileID)
				}
				return fmt.Errorf("档案 %s 在凭据库里没有账号 %q；有的账号：%s",
					profileID, name, strings.Join(available, "、"))
			}
			if err := store.Remove(profileID, name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已删除档案 %s 的账号 %s\n", profileID, name)
			return nil
		},
	}
	return cmd
}

// loadCredentialStore 按 XDG 配置目录读凭据库。
func loadCredentialStore() (*credentials.Store, error) {
	configDir, err := defaultConfigBaseDir()
	if err != nil {
		return nil, err
	}
	return credentials.Load(configDir)
}

// formatAddedAt 把加入时间排成人读的 RFC3339 秒精度。
//
// 存储用 RFC3339Nano 保证同一秒内多次写入的顺序可判；展示只需要秒。解析不了时
// 原样返回，手工改过的凭据库不该让整份清单读不出来。
func formatAddedAt(text string) string {
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return text
	}
	return parsed.Format(time.RFC3339)
}

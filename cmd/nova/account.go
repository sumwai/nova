package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/keystore"
)

func newAccountCmd() *cobra.Command {
	var show bool
	cmd := &cobra.Command{
		Use:   "account",
		Short: "列出凭据库里的账号",
		Long: `逐行列出凭据库里的账号：供应商名、账号名、加入时间与密钥尾四位。

缺省不回显完整密钥，只给尾四位；--show 才打印完整密钥。
本命令只读凭据库，不读配置、不联网。`,
		Args: rejectExtraArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := loadKeystore()
			if err != nil {
				return err
			}
			entries := store.All()
			if len(entries) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(),
					"凭据库里没有账号；可跑 `nova login` 添加")
				return nil
			}
			// 列对齐用 tabwriter：账号名长短不一，纯空格分隔在几行之后就读不出哪一列是哪一列。
			// 用制表符而不是固定宽度，是因为脱敏后的密钥与 --show 的完整密钥长度差很多。
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "供应商\t账号\t加入时间\t密钥")
			for _, entry := range entries {
				key := keystore.Mask(entry.Key)
				if show {
					key = entry.Key
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					entry.Namespace, entry.Name, formatAddedAt(entry.AddedAt), key)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&show, "show", false, "打印完整密钥（缺省只显示尾四位）")
	return cmd
}

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/version"
)

// versionReport 是 `nova version --json` 的输出结构。
//
// 字段名用 snake_case 而不是 Go 的惯例：这份输出的读者是脚本与别的程序，
// 而命令行工具的 JSON 惯例是 snake_case，与 JSON 生态的普遍写法也一致。
type versionReport struct {
	Version      string   `json:"version"`
	Revision     string   `json:"revision,omitempty"`
	CommitTime   string   `json:"commit_time,omitempty"`
	Modified     bool     `json:"modified"`
	GoVersion    string   `json:"go_version"`
	ConfigSchema []int    `json:"config_schemas"`
	Instructions []string `json:"instructions"`
}

func newVersionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "打印版本信息",
		Long: `打印本二进制的版本事实。

除版本号与提交之外，它还会报出本二进制实现哪几代配置语法、认识哪些配置指令。
这两条是「我这份配置能不能被这个 nova 读」的直接答案：不必翻源码，
也不必对着一串版本号猜。`,
		Args: rejectExtraArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printVersion(cmd.OutOrStdout(), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, longJSON, false, "以 JSON 输出，便于脚本消费")
	return cmd
}

// printVersion 输出版本事实，供 `nova version` 与 `nova --version` 共用。
//
// 人读形态里，取不到的事实整行省略而不打「未知」：三行里有两行写着「未知」时，
// 读者要费一番劲才能看出究竟哪一条才是真正缺失的。
func printVersion(w io.Writer, asJSON bool) error {
	info := version.Current()
	schemas := config.SupportedSchemas()
	instructions := config.InstructionNames()

	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(versionReport{
			Version:      info.Version,
			Revision:     info.Revision,
			CommitTime:   info.CommitTime,
			Modified:     info.Modified,
			GoVersion:    info.GoVersion,
			ConfigSchema: schemas,
			Instructions: instructions,
		})
	}

	_, _ = fmt.Fprintf(w, "nova %s (go %s)\n", info.Version, info.GoVersion)
	if info.Revision != "" {
		_, _ = fmt.Fprintf(w, "提交 %s%s\n",
			version.ShortRevision(info.Revision), dirtyMark(info.Modified))
	}
	_, _ = fmt.Fprintf(w, "配置语法代数 %s\n", joinInts(schemas))
	_, _ = fmt.Fprintf(w, "配置指令 %s\n", strings.Join(instructions, "、"))
	return nil
}

// dirtyMark 把「工作区已改动」补成提交行的后缀。
//
// 干净时返回空串：干净的构建是常态，给它留一句「（工作区干净）」只会让每个
// 读者都要多扫一眼才能确认无事发生。
func dirtyMark(modified bool) string {
	if modified {
		return "（工作区已改动）"
	}
	return ""
}

// joinInts 把代数列表排成「1」或「1、2」。
func joinInts(values []int) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, "、")
}

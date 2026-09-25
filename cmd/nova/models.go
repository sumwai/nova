package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/gateway"
)

func newModelsCmd() *cobra.Command {
	var configPath string
	var provider string
	var verbose bool
	cmd := &cobra.Command{
		Use:   "models",
		Short: "列出网关会认哪些模型（会连上游）",
		Long: `按配置装配一次模型目录并打印结果：每条发现型端点的清单统计与保留、排除的模型 id，
再加一行目录汇总。

它要连上游取清单，因此与 config check 分开：check 不联网，只校验配置写法。
用 --provider 只看一条渠道，用 --verbose 给保留项附上来源标注。`,
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
			// 名字先校验：过滤到一个不存在的渠道时，静默给一份空报告会让人以为
			// 「这条渠道一个模型都没有」，而真相是名字写错了。
			if provider != "" && !hasProvider(cfg, provider) {
				return fmt.Errorf("配置里没有名为 %q 的 provider；可用的是 %s",
					provider, providerNames(cfg))
			}
			// 发现过程的日志不另写 stderr：结果全部在下面这份报告里，
			// 两处各说一遍只会让同一件事在屏幕上出现两次。
			assembled, err := gateway.Assemble(cmd.Context(), cfg, gateway.AssembleOptions{LogOutput: io.Discard})
			if err != nil {
				return err
			}
			defer func() { _ = assembled.Close() }()

			printModels(cmd.OutOrStdout(), cfg, assembled, provider, verbose)
			return nil
		},
	}
	registerConfigFlag(cmd, &configPath)
	cmd.Flags().StringVar(&provider, "provider", "",
		"只显示指定 provider 的发现结果与模型（省略时显示全部）")
	cmd.Flags().BoolVar(&verbose, "verbose", false,
		"保留项附上来源标注（static / discovered）")
	return cmd
}

// hasProvider 报告配置里有没有这个名字的渠道。
func hasProvider(cfg *config.Config, name string) bool {
	for i := range cfg.Providers {
		if cfg.Providers[i].Name == name {
			return true
		}
	}
	return false
}

// providerNames 列出配置里的渠道名，用于「名字写错」时的提示。
func providerNames(cfg *config.Config) string {
	names := make([]string, 0, len(cfg.Providers))
	for i := range cfg.Providers {
		names = append(names, cfg.Providers[i].Name)
	}
	if len(names) == 0 {
		return "（配置里没有任何 provider）"
	}
	return strings.Join(names, ", ")
}

// printModels 把一次装配的目录打成报告。
//
// 单位是一条端点：先报发现统计，再分别列出保留与排除的模型 id。
// 只报计数在「某个模型为什么不能用」上帮不上忙——答案要么是保留列表里没有它，
// 要么是排除列表里有它，两组名单本身就是结论。
func printModels(w io.Writer, cfg *config.Config, assembled *gateway.Assembly, provider string, verbose bool) {
	entries := reportModels(cfg, assembled, provider)
	sources := make(map[string]gateway.ModelSource, len(entries))
	for _, entry := range entries {
		sources[entry.Name] = entry.Source
	}

	printed := false
	for _, report := range assembled.Discoveries {
		if provider != "" && report.Provider != provider {
			continue
		}
		if printed {
			_, _ = fmt.Fprintln(w)
		}
		printed = true
		printReport(w, report, sources, verbose)
	}

	// 指定的渠道没有声明 discover 时报告里没有它的端点段，这时把它的显式模型
	// 直接列出来：只回一句「没有发现」没法回答「那它到底提供什么」。
	if provider != "" && !printed {
		_, _ = fmt.Fprintf(w, "%s  没有声明 discover：模型全部来自显式声明的 model\n", provider)
		printModelsList(w, "显式声明", declaredModels(cfg, provider), nil)
		printed = true
	}

	if printed {
		_, _ = fmt.Fprintln(w)
	}
	static, discovered := countSources(entries)
	// 过滤到一条渠道时汇总只算这条渠道，并在文案里点明，免得被读成整份目录的总数。
	if provider == "" {
		_, _ = fmt.Fprintf(w, "对外模型 %d 个（显式 %d 个，发现 %d 个）\n",
			len(entries), static, discovered)
		return
	}
	_, _ = fmt.Fprintf(w, "provider %s 的对外模型 %d 个（显式 %d 个，发现 %d 个）\n",
		provider, len(entries), static, discovered)
}

// printReport 渲染一条端点的发现结果。
func printReport(
	w io.Writer,
	report gateway.DiscoveryReport,
	sources map[string]gateway.ModelSource,
	verbose bool,
) {
	if report.Err != nil {
		_, _ = fmt.Fprintf(w, "%s  %s  发现失败  %s\n",
			report.Provider, report.Listing, report.Duration.Round(time.Millisecond))
		_, _ = fmt.Fprintf(w, "  原因  %v\n", report.Err)
		return
	}
	_, _ = fmt.Fprintf(w, "%s  %s  %s  发现 %d 保留 %d 排除 %d  %s\n",
		report.Provider, report.Listing, report.Shape,
		report.Found, len(report.Kept), report.Filtered, report.Duration.Round(time.Millisecond))
	if report.HasMore {
		_, _ = fmt.Fprintln(w, "  清单自述还有下一页，本版不翻页")
	}
	// 显式声明排在保留之前：目录里它们的顺序也是如此，而且别名映射
	// （model gpt-4o openai/gpt-4o）只能在这一段里被看见。
	printModelsList(w, "显式声明", report.Declared, nil)
	printModelsList(w, "保留", report.Kept, func(name string) string {
		if !verbose {
			return ""
		}
		return string(sources[name])
	})
	printModelList(w, "排除", report.Excluded, nil)
	// 未命中的改名规则单独一段：它多半意味着模式写错，
	// 或者规则针对的项被 allow / deny 先拦掉了。
	if len(report.UnusedAliases) > 0 {
		_, _ = fmt.Fprintf(w, "\n  未命中的改名规则 %d\n", len(report.UnusedAliases))
		for _, alias := range report.UnusedAliases {
			_, _ = fmt.Fprintf(w, "    expose %s %s\n", alias.From, alias.To)
		}
	}
}

// printModelList 打印一组模型 id。
//
// 标题带条数、内容缩进一级：一屏里可能有几百个 id，标题给出总量，内容便于按行 grep。
// annotate 用来给每行加备注（例如来源），为 nil 时不加。
func printModelList(w io.Writer, title string, ids []string, annotate func(string) string) {
	if len(ids) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "\n  %s %d\n", title, len(ids))
	for _, id := range ids {
		note := ""
		if annotate != nil {
			note = annotate(id)
		}
		if note == "" {
			_, _ = fmt.Fprintf(w, "    %s\n", id)
			continue
		}
		_, _ = fmt.Fprintf(w, "    %s  %s\n", id, note)
	}
}

// reportModels 返回本次报告要统计的对外模型。
//
// 没有 --provider 时就是整份目录；指定渠道时只算这条渠道的模型：
// 显式声明的那些，加上它各端点的发现结果，同名以显式声明为准。
func reportModels(cfg *config.Config, assembled *gateway.Assembly, provider string) []gateway.ModelEntry {
	if provider == "" {
		return assembled.Models
	}

	var entries []gateway.ModelEntry
	seen := make(map[string]bool)
	add := func(name string, source gateway.ModelSource) {
		if seen[name] {
			return
		}
		seen[name] = true
		entries = append(entries, gateway.ModelEntry{Name: name, Source: source})
	}

	// 显式声明在前、发现项在后：与 gateway 构造目录时的顺序口径一致。
	for i := range cfg.Providers {
		if cfg.Providers[i].Name != provider {
			continue
		}
		for j := range cfg.Providers[i].Endpoints {
			for _, model := range cfg.Providers[i].Endpoints[j].Models {
				add(model.Name, gateway.ModelSourceStatic)
			}
		}
	}
	for _, report := range assembled.Discoveries {
		if report.Provider != provider {
			continue
		}
		for _, model := range report.Kept {
			add(model.Name, gateway.ModelSourceDiscovered)
		}
	}
	return entries
}

// printModelsList 打印一组模型：对外名在前，与上游名不同时标出箭头。
//
// 箭头是报告里很关键的一列：`expose` 与 `model` 的双记号都在做「客户端请求的名字 ≠
// 发往上游的名字」这件事，少了它就回答不了「这个模型到底打给上游的哪个名字」。
// 对外名与上游名相同时不加备注；annotate 为 nil 时不加额外备注。
func printModelsList(w io.Writer, title string, models []config.Model, annotate func(string) string) {
	if len(models) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "\n  %s %d\n", title, len(models))
	for _, model := range models {
		note := ""
		if model.Upstream != "" && model.Upstream != model.Name {
			note = "→ " + model.Upstream
		}
		if annotate != nil {
			if extra := annotate(model.Name); extra != "" {
				note = joinNote(note, extra)
			}
		}
		if note == "" {
			_, _ = fmt.Fprintf(w, "    %s\n", model.Name)
			continue
		}
		_, _ = fmt.Fprintf(w, "    %s  %s\n", model.Name, note)
	}
}

// joinNote 拼接两段备注，空的那段不参与。
func joinNote(first, second string) string {
	if first == "" {
		return second
	}
	if second == "" {
		return first
	}
	return first + "  " + second
}

// declaredModels 汇总一条渠道显式声明的模型，按声明顺序去重。
func declaredModels(cfg *config.Config, provider string) []config.Model {
	var models []config.Model
	seen := make(map[string]bool)
	for i := range cfg.Providers {
		if cfg.Providers[i].Name != provider {
			continue
		}
		for j := range cfg.Providers[i].Endpoints {
			for _, model := range cfg.Providers[i].Endpoints[j].Models {
				if seen[model.Name] {
					continue
				}
				seen[model.Name] = true
				models = append(models, model)
			}
		}
	}
	return models
}

// countSources 数出一份模型清单里的显式声明与发现项。
func countSources(entries []gateway.ModelEntry) (static, discovered int) {
	for _, entry := range entries {
		if entry.Source == gateway.ModelSourceStatic {
			static++
			continue
		}
		discovered++
	}
	return static, discovered
}

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/profile"
	"github.com/sumwai/nova/internal/profileapply"
)

func newProfilesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profiles",
		Short: "平台档案源相关操作",
		Args:  rejectExtraArgs,
		// 只给块名不给子命令时打帮助，与 config 同一口径：使用者多半在找下一步敲什么。
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newProfilesUpdateCmd(), newProfilesListCmd())
	return cmd
}

func newProfilesUpdateCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "同步远端档案源",
		Long: `按配置里的 profiles 块逐个处理档案源，把结论打成一行一个源。

远端源经 https 拉取、验签并按 serial 单调安装；builtin 随二进制发布，本地源直接读磁盘，
两者都不联网。某个源失败不会中断后面的源：一次跑完才能知道全部情况，
但只要有一个源失败，本命令就以非 0 退出。

输出到 stdout 的是给人看的结果，失败详情与提醒写 stderr。`,
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
			stateDir, err := defaultStateDir()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return profilesUpdate(ctx, cfg, stateDir, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	registerConfigFlag(cmd, &configPath)
	return cmd
}

func newProfilesListCmd() *cobra.Command {
	var configPath string
	var verbose bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出各档案源与已装快照",
		Long: `列出配置里每个档案源的种类、地址、已装 serial、上次成功更新时间与档案 id。

它不联网：远端源读的是状态目录下的 state.json 与已装快照目录，从未同步过的源显示
「尚未同步」而不是报错；本地源直接读磁盘上的目录。

用 --verbose 额外列出每份档案的 id、name 与 updated_at。`,
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
			stateDir, err := defaultStateDir()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return profilesList(ctx, cfg, stateDir, cmd.OutOrStdout(), cmd.ErrOrStderr(), verbose)
		},
	}
	registerConfigFlag(cmd, &configPath)
	cmd.Flags().BoolVar(&verbose, "verbose", false,
		"额外列出每份档案的 id、name 与 updated_at")
	return cmd
}

// sourceLabel 把一个源排成人读标签：种类 + 地址（本地为路径）。
func sourceLabel(src profile.Source) string {
	switch src.Kind {
	case profile.SourceBuiltin:
		return "内置源"
	case profile.SourceLocal:
		return "本地源 " + src.Path
	default:
		return "远端源 " + src.URL
	}
}

// profilesUpdate 逐个处理配置里的档案源。
//
// 不因一个源失败而中断：一次运行要能回答「哪些源成功、哪些失败」，因此失败只记数，
// 全部跑完后以汇总错误让退出码非 0。builtin 与本地源都不联网。
func profilesUpdate(
	ctx context.Context,
	cfg *config.Config,
	stateDir string,
	stdout, stderr io.Writer,
) error {
	syncer := &profile.Syncer{StateDir: stateDir}
	failed := 0

	for _, converted := range profileapply.ToSources(cfg.Profiles.Sources) {
		if converted.Kind == profile.SourceBuiltin {
			// builtin 随二进制发布，复制一份到状态目录不会多出任何保证。
			_, _ = fmt.Fprintf(stdout, "%s 随二进制发布，不联网拉取\n", sourceLabel(converted))
			continue
		}

		start := time.Now()
		snap, warnings, err := syncer.Resolve(ctx, converted)
		elapsed := time.Since(start).Round(time.Millisecond)
		// 提醒不阻止本次解析，但「更新被拒、旧快照仍生效」这类事实必须说出来。
		for _, warning := range warnings {
			_, _ = fmt.Fprintf(stderr, "提醒 %s\n", warning.String())
		}
		if err != nil {
			failed++
			_, _ = fmt.Fprintf(stdout, "%s 失败\n", sourceLabel(converted))
			_, _ = fmt.Fprintf(stderr, "失败 %s：%v\n", sourceAddress(converted), err)
			continue
		}
		_, _ = fmt.Fprintf(stdout, "%s %s\n", sourceLabel(converted), snapshotSummary(snap, elapsed))
	}

	if failed > 0 {
		return fmt.Errorf("%d 个源同步失败", failed)
	}
	return nil
}

// sourceAddress 取一个源的地址，用于失败行的定位。
func sourceAddress(src profile.Source) string {
	if src.Kind == profile.SourceLocal {
		return src.Path
	}
	return src.URL
}

// snapshotSummary 把一次成功解析排成「serial / 档案数 / 耗时」。
//
// 本地源没有索引，因此没有 serial 可报，这一项整段省略而不是打一个 0：
// 打 0 会被读成「序号是 0 的快照」。
func snapshotSummary(snap *profile.Snapshot, elapsed time.Duration) string {
	var parts []string
	if snap.Index != nil {
		parts = append(parts, fmt.Sprintf("serial %d", snap.Serial))
	}
	parts = append(parts, fmt.Sprintf("档案 %d 份", len(snap.Profiles)))
	parts = append(parts, "耗时 "+elapsed.String())
	return strings.Join(parts, " ")
}

// profilesList 列出每个源与其已装快照，全程不联网。
//
// 远端源只读状态目录：state.json 给出 serial、上次成功更新时间与快照目录，
// 目录里的档案再逐份 Load，因此这里不会重新拉取、也不会重跑一次同步。
//
// 与 update 同口径：一个源读失败不中断后面的源，最后汇总非 0。list 只是只读展示，
// 磁盘上一份快照被外部删掉不该让整份清单不可用。
func profilesList(
	ctx context.Context,
	cfg *config.Config,
	stateDir string,
	stdout, stderr io.Writer,
	verbose bool,
) error {
	syncer := &profile.Syncer{StateDir: stateDir}
	failed := 0

	for _, converted := range profileapply.ToSources(cfg.Profiles.Sources) {
		switch converted.Kind {
		case profile.SourceBuiltin:
			_, _ = fmt.Fprintf(stdout, "%s 随二进制发布，不经过同步\n", sourceLabel(converted))

		case profile.SourceLocal:
			// 本地源就在磁盘上，读它不涉及网络，也不算「安装」。
			snap, warnings, err := syncer.Resolve(ctx, converted)
			if err != nil {
				failed++
				_, _ = fmt.Fprintf(stdout, "%s 读取失败\n", sourceLabel(converted))
				_, _ = fmt.Fprintf(stderr, "失败 %s：%v\n", sourceAddress(converted), err)
				continue
			}
			for _, warning := range warnings {
				_, _ = fmt.Fprintf(stderr, "提醒 %s\n", warning.String())
			}
			printInstalled(stdout, sourceLabel(converted), false, 0, "", snap.Profiles, verbose)

		case profile.SourceRemote:
			installed, err := syncer.Installed(converted)
			if err != nil {
				failed++
				_, _ = fmt.Fprintf(stdout, "%s 读取失败\n", sourceLabel(converted))
				_, _ = fmt.Fprintf(stderr, "失败 %s：%v\n", sourceAddress(converted), err)
				continue
			}
			if installed == nil {
				// 从未同步过不是错误：新加的源本来就还没有本地快照。
				_, _ = fmt.Fprintf(stdout, "%s 尚未同步\n", sourceLabel(converted))
				continue
			}
			printInstalled(stdout, sourceLabel(converted), true,
				installed.Serial, installed.InstalledAt, installed.Profiles, verbose)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d 个源读取失败", failed)
	}
	return nil
}

// printInstalled 渲染一个源的已装快照。
//
// hasSerial 显式区分「没有序号」（本地源）与「序号就是 0」（远端源的合法起点）。
// 非 verbose 时档案只列 id；verbose 时每份再给一行 id / name / updated_at。
func printInstalled(
	w io.Writer,
	label string,
	hasSerial bool,
	serial int64,
	installedAt string,
	profiles []*profile.Profile,
	verbose bool,
) {
	var parts []string
	parts = append(parts, label)
	if hasSerial {
		parts = append(parts, fmt.Sprintf("serial %d", serial))
		if installedAt != "" {
			parts = append(parts, "上次更新 "+installedAt)
		}
	}
	parts = append(parts, fmt.Sprintf("档案 %d 份", len(profiles)))
	if ids := profileIDs(profiles); ids != "" {
		parts[len(parts)-1] += "：" + ids
	}
	_, _ = fmt.Fprintln(w, strings.Join(parts, "  "))

	if !verbose {
		return
	}
	for _, loaded := range profiles {
		fields := []string{"  " + loaded.ID}
		if loaded.Name != "" {
			fields = append(fields, loaded.Name)
		}
		if loaded.UpdatedAt != "" {
			fields = append(fields, loaded.UpdatedAt)
		}
		_, _ = fmt.Fprintln(w, strings.Join(fields, "  "))
	}
}

// profileIDs 把档案 id 排成「a、b、c」，顺序即传入顺序。
func profileIDs(profiles []*profile.Profile) string {
	ids := make([]string, 0, len(profiles))
	for _, loaded := range profiles {
		ids = append(ids, loaded.ID)
	}
	return strings.Join(ids, "、")
}

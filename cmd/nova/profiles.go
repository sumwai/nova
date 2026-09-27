package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/profile"
	"github.com/sumwai/nova/internal/profileapply"
	"github.com/sumwai/nova/internal/sourceurl"
)

func newProfilesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profiles",
		Short: "平台档案源相关操作",
		Args:  rejectExtraArgs,
		// 只给块名不给子命令时打帮助，与 config 同一口径：使用者多半在找下一步敲什么。
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newProfilesUpdateCmd(), newProfilesListCmd(), newProfilesVerifyCmd(), newProfilesDiffCmd())
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

func newProfilesVerifyCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "verify [<源>]",
		Short: "完整校验一个档案源，不安装、不改本机状态",
		Long: `对指定的源跑一遍与同步完全相同的校验：远端源取索引与签名、验签、检查 serial 与
not_after、逐份下载并校验 sha256 与 schema、核对文件集合与 id；本地源逐份解析目录。

与 update 的差别只有一处：校验用临时目录承载下载结果，不写状态、不动已装快照，
因此可以反复跑，也可以对一份刚改过发布的源先看一眼。不带 <源> 时校验配置里的全部源。

<源> 接受内置标记、本地源目录或远端源地址；地址按同一套规则归一后匹配。
结论写 stdout，提醒与失败详情写 stderr；任一源校验失败即非 0 退出。`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			return profilesVerify(ctx, cfg, stateDir, cmd.OutOrStdout(), cmd.ErrOrStderr(), firstArg(args))
		},
	}
	registerConfigFlag(cmd, &configPath)
	return cmd
}

func newProfilesDiffCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "diff [<源>]",
		Short: "打印待装快照相对已装快照的变更摘要",
		Long: `对每个远端源重跑一次校验，把待装快照与已装快照按档案 id 对比，列出端点地址与协议、
认证头与静态头、探测命令的增删改。已装快照只读本地状态，待装快照用临时目录承载，
两者都不改本机状态。

本地源与内置源没有「已装 vs 待装」这层区分，会直接说明并跳过。不带 <源> 时对比全部源。`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			return profilesDiff(ctx, cfg, stateDir, cmd.OutOrStdout(), cmd.ErrOrStderr(), firstArg(args))
		},
	}
	registerConfigFlag(cmd, &configPath)
	return cmd
}

// firstArg 取可选的位置参数，未给出时返回空串。
func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// selectSources 按 CLI 给出的源筛选配置里的源；target 为空时返回全部。
//
// 源按「归一后的地址 / 目录路径 / builtin 字样」匹配：同一个远端地址的不同写法
// 应当能定位到同一个源，而这份归一规则只在 internal/sourceurl 里维护一份。
func selectSources(sources []profile.Source, target string) ([]profile.Source, error) {
	if target == "" {
		return sources, nil
	}
	for _, src := range sources {
		if sourceMatches(src, target) {
			return []profile.Source{src}, nil
		}
	}
	labels := make([]string, 0, len(sources))
	for _, src := range sources {
		labels = append(labels, sourceLabel(src))
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf("配置里没有声明任何档案源，无法校验 %q", target)
	}
	return nil, fmt.Errorf("配置里没有源 %q；已声明的源：%s", target, strings.Join(labels, "、"))
}

// sourceMatches 判断一个 CLI 源参数是否指向配置里的某个源。
func sourceMatches(src profile.Source, target string) bool {
	switch src.Kind {
	case profile.SourceBuiltin:
		return target == "builtin"
	case profile.SourceLocal:
		return target == src.Path
	default:
		return sourceurl.Normalize(target) == sourceurl.Normalize(src.URL)
	}
}

// verifySource 在不改动本机状态的前提下完整解析一个源。
//
// 远端源用临时 StateDir 承载「安装」：同步的每一道校验都照跑，但结果落在临时目录，
// 既不写 state.json，也不碰已装快照。函数返回前临时目录已删除，返回的快照里只有
// 内存中的档案对象，Root 不再指向存在的路径。
func verifySource(ctx context.Context, src profile.Source, stateDir string) (*profile.Snapshot, []profile.Warning, error) {
	if src.Kind != profile.SourceRemote {
		return (&profile.Syncer{StateDir: stateDir}).Resolve(ctx, src)
	}
	temp, err := os.MkdirTemp("", "nova-verify-")
	if err != nil {
		return nil, nil, fmt.Errorf("创建校验用临时目录失败：%w", err)
	}
	defer os.RemoveAll(temp)
	return (&profile.Syncer{StateDir: temp}).Resolve(ctx, src)
}

// profilesVerify 逐个完整校验配置里的源，不安装。
func profilesVerify(
	ctx context.Context,
	cfg *config.Config,
	stateDir string,
	stdout, stderr io.Writer,
	target string,
) error {
	sources, err := selectSources(profileapply.ToSources(cfg.Profiles.Sources), target)
	if err != nil {
		return err
	}
	failed := 0

	for _, src := range sources {
		if src.Kind == profile.SourceBuiltin {
			builtin, builtinErr := profile.Builtin()
			if builtinErr != nil {
				failed++
				_, _ = fmt.Fprintf(stdout, "%s 校验失败\n", sourceLabel(src))
				_, _ = fmt.Fprintf(stderr, "失败 %s：%v\n", sourceAddress(src), builtinErr)
				continue
			}
			_, _ = fmt.Fprintf(stdout, "%s 校验通过  档案 %d 份：%s\n",
				sourceLabel(src), len(builtin), profileIDs(builtin))
			continue
		}

		snap, warnings, resolveErr := verifySource(ctx, src, stateDir)
		for _, warning := range warnings {
			_, _ = fmt.Fprintf(stderr, "提醒 %s\n", warning.String())
		}
		if resolveErr != nil {
			failed++
			_, _ = fmt.Fprintf(stdout, "%s 校验失败\n", sourceLabel(src))
			_, _ = fmt.Fprintf(stderr, "失败 %s：%v\n", sourceAddress(src), resolveErr)
			continue
		}
		printVerified(stdout, src, snap, stateDir)
	}

	if failed > 0 {
		return fmt.Errorf("%d 个源校验失败", failed)
	}
	return nil
}

// printVerified 渲染一个源通过校验后的结论。
//
// 远端源额外报告它与本机已装快照的序号关系：序号不大于已装值时 update 会拒绝，
// 这件事只能在「本机已装状态」与「远端当前索引」之间比较，因此放在校验结论里。
func printVerified(w io.Writer, src profile.Source, snap *profile.Snapshot, stateDir string) {
	parts := []string{sourceLabel(src), "校验通过"}
	if snap.Index != nil {
		parts = append(parts, fmt.Sprintf("serial %d", snap.Serial))
	}
	parts = append(parts, fmt.Sprintf("档案 %d 份：%s", len(snap.Profiles), profileIDs(snap.Profiles)))
	_, _ = fmt.Fprintln(w, strings.Join(parts, "  "))

	if snap.Index != nil {
		installed, err := (&profile.Syncer{StateDir: stateDir}).Installed(src)
		switch {
		case err != nil:
			_, _ = fmt.Fprintf(w, "  已装快照读取失败，无法判断序号关系：%v\n", err)
		case installed == nil:
			_, _ = fmt.Fprintf(w, "  本机尚未同步过该源，首次同步会直接安装 serial %d\n", snap.Serial)
		case snap.Serial > installed.Serial:
			_, _ = fmt.Fprintf(w, "  本机已装 serial %d，update 会替换为 serial %d\n", installed.Serial, snap.Serial)
		default:
			_, _ = fmt.Fprintf(w, "  本机已装 serial %d，serial %d 不大于它，update 会拒绝\n", installed.Serial, snap.Serial)
		}
	}

	for _, loaded := range snap.Profiles {
		_, _ = fmt.Fprintf(w, "  %s  %s  端点 %d  模型 %d  计划 %d\n",
			loaded.ID, loaded.Name, len(loaded.Endpoints), len(loaded.Models), len(loaded.Plans))
	}
}

// profilesDiff 打印每个远端源「已装 → 待装」的变更摘要。
func profilesDiff(
	ctx context.Context,
	cfg *config.Config,
	stateDir string,
	stdout, stderr io.Writer,
	target string,
) error {
	sources, err := selectSources(profileapply.ToSources(cfg.Profiles.Sources), target)
	if err != nil {
		return err
	}
	failed := 0

	for _, src := range sources {
		if src.Kind != profile.SourceRemote {
			_, _ = fmt.Fprintf(stdout, "%s 不是远端源，没有「已装 vs 待装」可对比\n", sourceLabel(src))
			continue
		}

		installed, err := (&profile.Syncer{StateDir: stateDir}).Installed(src)
		if err != nil {
			failed++
			_, _ = fmt.Fprintf(stdout, "%s 读取失败\n", sourceLabel(src))
			_, _ = fmt.Fprintf(stderr, "失败 %s：%v\n", sourceAddress(src), err)
			continue
		}
		next, warnings, resolveErr := verifySource(ctx, src, stateDir)
		for _, warning := range warnings {
			_, _ = fmt.Fprintf(stderr, "提醒 %s\n", warning.String())
		}
		if resolveErr != nil {
			failed++
			_, _ = fmt.Fprintf(stdout, "%s 校验失败\n", sourceLabel(src))
			_, _ = fmt.Fprintf(stderr, "失败 %s：%v\n", sourceAddress(src), resolveErr)
			continue
		}

		if installed == nil {
			_, _ = fmt.Fprintf(stdout, "%s 尚未同步；待装 serial %d 含 %d 份档案（全部为新增）\n",
				sourceLabel(src), next.Serial, len(next.Profiles))
			for _, line := range diffProfiles(nil, next.Profiles) {
				_, _ = fmt.Fprintln(stdout, "  "+line)
			}
			continue
		}

		_, _ = fmt.Fprintf(stdout, "%s 已装 serial %d → 待装 serial %d\n",
			sourceLabel(src), installed.Serial, next.Serial)
		changes := diffProfiles(installed.Profiles, next.Profiles)
		if len(changes) == 0 {
			_, _ = fmt.Fprintln(stdout, "  端点、认证头、静态头与探测命令均无变化")
			continue
		}
		for _, line := range changes {
			_, _ = fmt.Fprintln(stdout, "  "+line)
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d 个源读取失败", failed)
	}
	return nil
}

// diffProfiles 按档案 id 比对两份快照，只列端点、认证、静态头与探测命令的变化。
//
// 模型集合与价格随索引发布天天变，逐条列出会淹没真正需要人工确认的几项：
// 端点地址、认证头与执行命令。删除与新增的档案整体列出。
func diffProfiles(previous, next []*profile.Profile) []string {
	prevByID := indexProfiles(previous)
	nextByID := indexProfiles(next)
	var out []string

	for _, id := range sortedIDs(prevByID) {
		if _, ok := nextByID[id]; !ok {
			out = append(out, fmt.Sprintf("删除档案 %s", id))
		}
	}
	for _, id := range sortedIDs(nextByID) {
		before, ok := prevByID[id]
		if !ok {
			out = append(out, fmt.Sprintf("新增档案 %s", id))
			continue
		}
		out = append(out, diffProfile(id, before, nextByID[id])...)
	}
	return out
}

// diffProfile 比对同一份档案的两个版本。
func diffProfile(id string, before, after *profile.Profile) []string {
	var out []string

	for _, name := range sortedEndpointNames(before, after) {
		oldEP, hadOld := before.Endpoints[name]
		newEP, hasNew := after.Endpoints[name]
		switch {
		case !hadOld && hasNew:
			out = append(out, fmt.Sprintf("档案 %s 新增端点 %s（%s %s）", id, name, newEP.Protocol, newEP.URL))
		case hadOld && !hasNew:
			out = append(out, fmt.Sprintf("档案 %s 删除端点 %s", id, name))
		case oldEP.URL != newEP.URL:
			out = append(out, fmt.Sprintf("档案 %s 端点 %s 地址：%s → %s", id, name, oldEP.URL, newEP.URL))
		}
		if hadOld && hasNew && oldEP.Protocol != newEP.Protocol {
			out = append(out, fmt.Sprintf("档案 %s 端点 %s 协议：%s → %s", id, name, oldEP.Protocol, newEP.Protocol))
		}
	}

	if before.Auth.Header != after.Auth.Header {
		out = append(out, fmt.Sprintf("档案 %s 认证头：%s → %s", id, orNone(before.Auth.Header), orNone(after.Auth.Header)))
	}
	if before.Auth.Scheme != after.Auth.Scheme {
		out = append(out, fmt.Sprintf("档案 %s 认证方案：%s → %s", id, orNone(before.Auth.Scheme), orNone(after.Auth.Scheme)))
	}
	if before.Auth.Env != after.Auth.Env {
		out = append(out, fmt.Sprintf("档案 %s 认证环境变量：%s → %s", id, orNone(before.Auth.Env), orNone(after.Auth.Env)))
	}

	for _, name := range sortedHeaderNames(before.Headers, after.Headers) {
		oldValue, hadOld := before.Headers[name]
		newValue, hasNew := after.Headers[name]
		switch {
		case !hadOld && hasNew:
			out = append(out, fmt.Sprintf("档案 %s 新增静态头 %s：%s", id, name, newValue))
		case hadOld && !hasNew:
			out = append(out, fmt.Sprintf("档案 %s 删除静态头 %s", id, name))
		case oldValue != newValue:
			out = append(out, fmt.Sprintf("档案 %s 静态头 %s：%s → %s", id, name, oldValue, newValue))
		}
	}

	oldProbe, oldExec := usageCommand(before)
	newProbe, newExec := usageCommand(after)
	if oldProbe != newProbe || oldExec != newExec {
		out = append(out, fmt.Sprintf("档案 %s 探测命令：%s → %s",
			id, describeCommand(oldProbe, oldExec), describeCommand(newProbe, newExec)))
	}

	return out
}

// indexProfiles 按 id 索引一份快照里的档案。
func indexProfiles(profiles []*profile.Profile) map[string]*profile.Profile {
	out := make(map[string]*profile.Profile, len(profiles))
	for _, loaded := range profiles {
		out[loaded.ID] = loaded
	}
	return out
}

// sortedIDs 返回一份档案索引里的 id，按字典序。
func sortedIDs(byID map[string]*profile.Profile) []string {
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// sortedEndpointNames 返回两份档案涉及的端点名并集，按字典序。
func sortedEndpointNames(before, after *profile.Profile) []string {
	seen := map[string]bool{}
	for name := range before.Endpoints {
		seen[name] = true
	}
	for name := range after.Endpoints {
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sortedHeaderNames 返回两份档案涉及的静态头名并集，按字典序。
func sortedHeaderNames(before, after map[string]string) []string {
	seen := map[string]bool{}
	for name := range before {
		seen[name] = true
	}
	for name := range after {
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// usageCommand 取一份档案声明的探测命令：内置 probe 名或外部 exec 路径。
func usageCommand(loaded *profile.Profile) (probe, exec string) {
	if loaded.Usage == nil {
		return "", ""
	}
	return loaded.Usage.Probe, loaded.Usage.Exec
}

// describeCommand 把人读的探测命令排成一行。
func describeCommand(probe, exec string) string {
	switch {
	case probe != "" && exec != "":
		return "probe " + probe + " / exec " + exec
	case probe != "":
		return "probe " + probe
	case exec != "":
		return "exec " + exec
	default:
		return "无"
	}
}

// orNone 把空串渲染成「无」，让「从无到有」在摘要里可读。
func orNone(value string) string {
	if value == "" {
		return "无"
	}
	return value
}

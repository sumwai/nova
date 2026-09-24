package gateway

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/transport"
)

// 本文件是人读模式（log_format text）的全部排版规则。
//
// 它与 json 模式的分工是「给人看」与「给机器看」：这里的每一次省略（同协议不写协议、
// 同模型不写模型、零值子项不写）都是为了让人一眼扫出哪条不对，代价是信息不再完整。
// 需要完整字段时应当切到 json 模式，而不是来补这里的省略——把省略一个个去掉，
// text 就退化成一行塞满键值的机器输出，那还不如直接用 json。

// ANSI 转义序列。只用到四个颜色，因此不引入终端能力库：引一棵依赖树来做四件事，
// 换来的是每次升级都要跟着看它的变更。
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
)

// parseLevel 把配置里的级别字面量映射为 slog.Level。
func parseLevel(raw string) (slog.Level, error) {
	switch raw {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("日志级别 %q 不认识，取值只能是 debug / info / warn / error", raw)
}

// parseFormat 把配置里的格式字面量映射为生效格式。
func parseFormat(raw string) (logFormat, error) {
	switch raw {
	case "text":
		return logText, nil
	case "json":
		return logJSON, nil
	}
	return 0, fmt.Errorf("日志格式 %q 不认识，取值只能是 text / json", raw)
}

// detectColor 报告这次输出要不要着色。
//
// 三个条件缺一不可：输出落在终端上、环境没有声明不要颜色、终端没有声明读不懂控制序列。
// 判定只用输出目标本身的状态，不做全局探测——日志走 stderr 而 stdout 被重定向是常见组合，
// 拿 stdout 的结论来决定 stderr 的颜色，会在半边管道里写出半边转义序列。
//
// 「是不是终端」用字符设备近似：它能分开文件与管道，代价是 /dev/null 也会被判成终端。
// 这个误差可以接受——把日志丢进 /dev/null 的人不会介意里面有没有颜色。
func detectColor(out io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	switch os.Getenv("TERM") {
	case "", "dumb":
		return false
	}
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// clockNow 是日志行开头的时间。只到秒，不带日期。
//
// 不带日期是有意的取舍：终端上看日志时日期是同一天的重复噪声，而落进 journald 或文件时
// 外部工具自会补上时间戳。需要带日期与纳秒的完整时刻时用 log_format json——
// 那条路走的是 RFC3339，本来就是给机器解析的。
func clockNow() string {
	return time.Now().Format("15:04:05")
}

// levelName 是级别在人读模式下的固定宽度标签。
//
// 宽度固定为 5 是为了让同一列上的级别对齐：级别不对齐时，视觉上要逐行读才能找出 WARN，
// 而「一眼看出哪条不对」正是这一列存在的意义。
func levelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARN "
	case level >= slog.LevelInfo:
		return "INFO "
	default:
		return "DEBUG"
	}
}

// paintLevel 渲染级别标签，color 为真时着色。
func paintLevel(level slog.Level, color bool) string {
	name := levelName(level)
	if !color {
		return name
	}
	switch {
	case level >= slog.LevelError:
		return ansiRed + name + ansiReset
	case level >= slog.LevelWarn:
		return ansiYellow + name + ansiReset
	case level >= slog.LevelDebug:
		return ansiDim + name + ansiReset
	default:
		return name
	}
}

// paintError 渲染错误码：它非空就说明这条记录里有要查的东西，因此跟着级别一起上色。
func paintError(code string, color bool, level slog.Level) string {
	if !color {
		return code
	}
	if level >= slog.LevelError {
		return ansiRed + code + ansiReset
	}
	return ansiYellow + code + ansiReset
}

// durationText 把耗时渲染成人眼好读的两种量级。
func durationText(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// usageText 渲染用量。
//
// 主计数写成 in/out 一对，子项只在非零时出现：绝大多数请求没有缓存与推理，
// 把三个零值子项每次都打出来，只会让真正有缓存命中的那几条淹没在噪声里。
// 用量未知时显式写 unknown，而不是打一个 0/0——后者看起来像「这次请求不花钱」，
// 与事实正好相反。
func usageText(u domain.Usage) string {
	if !u.Known() {
		return "usage unknown"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d/%d tok", u.InputTokens, u.OutputTokens)
	if u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
		fmt.Fprintf(&b, "  cache %d/%d", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.ReasoningTokens != 0 {
		fmt.Fprintf(&b, "  reason %d", u.ReasoningTokens)
	}
	if u.ServerToolUses != 0 {
		fmt.Fprintf(&b, "  tools %d", u.ServerToolUses)
	}
	return b.String()
}

// displayWidth 返回字符串在终端里占的列数：东亚宽字符算两列，其余算一列。
//
// 不能直接用 len([]rune(s)) 或 len(s)：汉字的 rune 计数是 1、字节数是 3，
// 占位却是 2 列。用错哪个数都会让横幅的标签列在第二行就开始错位。
//
// 判定用码点区间而不是 unicode 表：需要覆盖的只有 CJK 与全角形式这几段，
// 而它们的区间是稳定的；引入一个宽度库去做这几行判断并不划算。
func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		if isWideRune(r) {
			width += 2
			continue
		}
		width++
	}
	return width
}

func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F: // 谚文字母
	case r >= 0x2E80 && r <= 0xA4CF: // CJK 部首、汉字、假名
	case r >= 0xAC00 && r <= 0xD7A3: // 谚文音节
	case r >= 0xF900 && r <= 0xFAFF: // CJK 兼容汉字
	case r >= 0xFE30 && r <= 0xFE6F: // CJK 兼容形式
	case r >= 0xFF00 && r <= 0xFF60: // 全角形式
	case r >= 0xFFE0 && r <= 0xFFE6: // 全角符号
	default:
		return false
	}
	return true
}

// renderStartup 是人读模式的启动横幅。
//
// 它一次列清「这个进程现在用什么配置在跑」，而不是把这些事实拆进多条日志：排查的第一步
// 永远是确认现场，而这几个字段就是现场本身。渠道与模型只报数量不报名单——名单可能十几条，
// 会把这屏刷掉；要看全清单应该有个单独的命令，不是在启动时刷屏。
func renderStartup(cfg *config.Config, reloaded bool) string {
	headline := "nova 已启动"
	if reloaded {
		headline = "nova 已重载"
	}
	auth := "未启用"
	if len(cfg.ClientKeys) > 0 {
		auth = fmt.Sprintf("已启用（%d 个 client_key）", len(cfg.ClientKeys))
	}
	rows := [][2]string{
		{"配置文件", cfg.Path},
		{"配置代数", strconv.Itoa(cfg.Schema)},
		{"监听", cfg.Listen},
		{"管理端点", cfg.Admin},
		{"日志级别", cfg.LogLevel},
		{"日志格式", cfg.LogFormat},
		{"渠道", fmt.Sprintf("%d 条，对外模型 %d 个", len(cfg.Providers), len(cfg.ModelNames()))},
		{"客户端鉴权", auth},
	}
	var b strings.Builder
	b.WriteString(headline)
	for _, row := range rows {
		b.WriteString("\n  ")
		b.WriteString(row[0])
		b.WriteString(strings.Repeat(" ", labelWidth-displayWidth(row[0])+2))
		b.WriteString(row[1])
	}
	return b.String()
}

// labelWidth 是横幅里标签列的显示宽度，按最长的那个标签（「客户端鉴权」五个汉字＝10 列）取。
const labelWidth = 10

// renderWarning 渲染一条配置提醒。
func renderWarning(warn config.Warning, color bool) string {
	level := slog.LevelWarn
	return clockNow() + " " + paintLevel(level, color) + "  提醒  " + warn.String()
}

// joinFields 用两个空格连接非空片段。
//
// 空片段必须整段去掉而不是写个空串：被拒绝的请求没有 request_id、未注册路径
// 没有协议与模型名，把它们当空串占位写下去，行里就会冒出一串双空格，
// 看上去像排版坏了，而实际只是没东西可写。
func joinFields(fields ...string) string {
	kept := make([]string, 0, len(fields))
	for _, field := range fields {
		if field != "" {
			kept = append(kept, field)
		}
	}
	return strings.Join(kept, "  ")
}

// renderAccess 渲染一条访问日志。
//
// 字段顺序按「排查时先看哪个」排：先是关联键，再是「谁问了什么」，最后是结果与来源。
// 空值字段一律不出现，因此行内位置不是固定的——这是人读模式的固有代价，
// 需要固定字段位置时用 json 模式。
func renderAccess(record transport.AccessRecord, color bool) string {
	level := accessLevel(record.HTTPStatus)
	fields := []string{record.RequestID, string(record.Protocol), record.Model}
	if record.Stream {
		fields = append(fields, "stream")
	}
	fields = append(fields,
		strconv.Itoa(record.HTTPStatus),
		durationText(time.Duration(record.DurationMS)*time.Millisecond),
	)
	if record.ErrorCode != "" {
		fields = append(fields, paintError(record.ErrorCode, color, level))
	}
	// RemoteAddr 与 User-Agent 排在最后：它们最不重要却最长，
	// 放在中段会把结果字段挤到看不见的地方。
	fields = append(fields, record.RemoteAddr, record.UserAgent)
	return clockNow() + " " + paintLevel(level, color) + "  access  " + joinFields(fields...)
}

// renderAttempt 渲染一条上游尝试日志。
//
// 协议与模型只在真的不同时才写，且写成箭头：同协议同模型是最常见的情形，那时这两个字段
// 只是把访问日志里的名字重抄一遍。真的跨协议或改写模型时，「进了什么、出成什么」必须一眼可见。
//
// 上游明细（上游状态码与响应片段）另起一行缩进排：它是失败记录里唯一真正要看的东西，
// 挤在一行尾部等于没有。它也因此不受级别过滤之外的任何裁剪——超长时由生产端截断
// （见 internal/domain 对 ErrorDetail 的约定）。
func renderAttempt(rec domain.AttemptRecord, color bool) string {
	level := attemptLevel(rec.Outcome)
	fields := []string{rec.RequestID, "#" + strconv.Itoa(rec.Attempt), rec.UpstreamID}
	if rec.UpstreamProtocol != rec.ClientProtocol {
		fields = append(fields, fmt.Sprintf("%s→%s", rec.ClientProtocol, rec.UpstreamProtocol))
	}
	if rec.UpstreamModel != rec.RequestedModel {
		fields = append(fields, fmt.Sprintf("%s→%s", rec.RequestedModel, rec.UpstreamModel))
	}
	fields = append(fields, string(rec.Outcome), durationText(rec.EndedAt.Sub(rec.StartedAt)))
	if rec.ErrorCode != "" {
		fields = append(fields, paintError(rec.ErrorCode, color, level))
	}
	if rec.Usage.Known() {
		fields = append(fields, usageText(rec.Usage))
	}
	line := clockNow() + " " + paintLevel(level, color) + "  attempt  " + joinFields(fields...)
	if rec.ErrorDetail != "" {
		line += "\n    " + rec.ErrorDetail
	}
	return line
}

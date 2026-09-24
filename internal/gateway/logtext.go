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
// 同模型不写模型、零值子项不写、单次成功尝试不单独成行）都是为了让人一眼扫出哪条不对，
// 代价是信息不再完整。需要完整字段时应当切到 json 模式，而不是来补这里的省略——
// 把省略一个个去掉，text 就退化成一行塞满键值的机器输出，那还不如直接用 json。
//
// text 的排版单位是**一次请求一个块**：主行是 access（客户端侧终态），
// 其后是同一请求的上游明细（上游 id、两侧协议与模型、用量、上游报文）。
// 块内所有行共用主行的时刻与种类列宽度，字段起始列一致，因此能整块扫过；
// 块与块之间靠时间戳与关联键前缀分开。块由 logger 组装（见 log.go）。

// 级别标签之外的消息名。宽度固定（见 kindColumn），它们在行里的位置因此固定。
const (
	kindAccess   = "access"
	kindUpstream = "upstream"
	kindCatalog  = "catalog"
	kindReload   = "reload"
	kindWarning  = "提醒"
)

// kindWidth 是消息名列的显示宽度，取最长的一个（upstream）。
const kindWidth = 8

// kindColumn 渲染消息名列。
//
// 宽度固定是为了让字段起始列在所有记录上一致：消息名不等宽时，字段会随记录种类左右漂移，
// 而「一眼扫过一屏找出异常」正是固定列要买的东西。
func kindColumn(kind string) string {
	pad := kindWidth - displayWidth(kind)
	if pad < 0 {
		pad = 0
	}
	return "  " + kind + strings.Repeat(" ", pad) + "  "
}

// requestIDWidth 是人读模式里关联键的字符数。
//
// 关联键是 128 位随机标识，完整值 32 个字符，在一个主行加若干明细行的块里会重复出现多次。
// 截成前缀之后仍能用前缀 grep 命中（截断值就是完整值的前缀），碰撞概率在单个日志窗口内
// 可忽略。需要完整值时用 log_format json 或看该块的其它行。
const requestIDWidth = 8

func shortRequestID(id string) string {
	if len(id) <= requestIDWidth {
		return id
	}
	return id[:requestIDWidth]
}

// oneLineText 把上游报文压成一行。
//
// 明细行按「一条记录一行」读：报文里的换行会让一个块看起来像多了几条记录，
// 也会让按行 grep 的结果停在半截 JSON 上。报文本身是数据，压成一行只是排版处理。
func oneLineText(text string) string {
	if !strings.ContainsAny(text, "\r\n") {
		return text
	}
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text)
}

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
	return clockAt(time.Now())
}

// clockAt 把时刻渲染成行首的时间戳。
//
// 一个块里的所有行都用主行的时刻：明细与主行属于同一次请求，各写各的时刻会让它们看起来
// 是先后发生的两件事。
func clockAt(moment time.Time) string {
	return moment.Format("15:04:05")
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
func renderStartup(cfg *config.Config, stats catalogStats, reloaded bool) string {
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
		{"渠道", fmt.Sprintf("%d 条，对外模型 %d 个", len(cfg.Providers), stats.Models)},
	}
	// 账号池单独占一行，只在真的声明了多账号时出现：它回答「同一个渠道里的凭据
	// 是怎么选的」，而单账号配置没有这个问题需要回答。
	if pools, accounts := cfg.AccountPoolStats(); pools > 0 {
		rows = append(rows, [2]string{"账号池", fmt.Sprintf(
			"%d 个渠道共 %d 个账号，%s", pools, accounts, cfg.AccountPoolPolicyText())})
	}
	// 发现相关的两行只在真的发生时才出现：不声明发现的配置与以前一字不差，
	// 而声明了发现的配置需要一眼看出「清单给了多少、留下多少、有没有退回去」。
	if stats.DiscoveryEndpoints > 0 {
		rows = append(rows, [2]string{"模型发现", fmt.Sprintf(
			"%d 条端点，清单 %d 条 → 保留 %d 条（排除 %d）",
			stats.DiscoveryEndpoints, stats.Discovered, stats.Kept, stats.Filtered)})
	}
	if stats.Degraded > 0 {
		rows = append(rows, [2]string{"发现降级", fmt.Sprintf(
			"%d 条端点发现失败，只用显式声明的模型", stats.Degraded)})
	}
	rows = append(rows, [2]string{"客户端鉴权", auth})

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
	return clockNow() + " " + paintLevel(level, color) + kindColumn(kindWarning) + warn.String()
}

// renderDiscovery 渲染一条端点发现记录。
//
// 字段顺序与其它记录一致：先定位（渠道、清单地址），再说结果（风格、发现 / 保留 / 排除），
// 然后是耗时与降级处置，最后是客户端可用的对外名名单。名单列在行上是故意的：
// 启动时最常问的问题就是「网关现在认哪些模型」，答案应当在这一行里就能读到。
// 用的是对外名而不是上游 id：`expose` 改名后，客户端看到的名字与上游的名字是两回事。
func renderDiscovery(clock string, rec DiscoveryReport, color bool) string {
	fields := []string{rec.Provider, rec.Listing}
	if rec.Err != nil {
		fields = append(fields, "发现失败")
	} else {
		fields = append(fields, string(rec.Shape),
			fmt.Sprintf("发现 %d 保留 %d 排除 %d", rec.Found, len(rec.Kept), rec.Filtered))
		if rec.HasMore {
			fields = append(fields, "清单还有下一页")
		}
	}
	fields = append(fields, durationText(rec.Duration), degradedText(rec))
	if rec.Err != nil {
		fields = append(fields, oneLineText(rec.Err.Error()))
	} else {
		// 显式声明的模型不在清单视角里，但同样是客户端可用的名字。少了这一段，
		// 日志会看起来像「配的 model 行没生效」——一个只有清单的世界。
		fields = append(fields, namesText("显式：", rec.Declared))
		fields = append(fields, namesText("保留：", rec.Kept))
	}
	return clock + " " + paintLevel(discoveryLevel(rec), color) +
		kindColumn(kindCatalog) + joinFields(fields...)
}

// discoveryNameLimit 是 catalog 记录里逐条列出的对外名上限。
//
// 一行日志的可读长度有限，而一个中转站可能有几百个模型。超出时只列前若干个
// 并给出总数，全量名单由 `nova models` 回答；json 模式下不截断，那边是给机器读的。
const discoveryNameLimit = 20

// namesText 把一组对外名排成一段，超限时截断并给出总数。
//
// 显式声明的模型与清单保留项各成一段：它们的来源不同，混在一起会丢掉
// 「这个名字是配下的还是上游给的」这一事实，而那正是排查选路时要看的。
func namesText(label string, models []config.Model) string {
	if len(models) == 0 {
		return ""
	}
	names := make([]string, 0, len(models))
	for i, model := range models {
		if i >= discoveryNameLimit {
			break
		}
		names = append(names, model.Name)
	}
	text := label + strings.Join(names, " ")
	if len(models) > discoveryNameLimit {
		text += fmt.Sprintf(" …（共 %d 个）", len(models))
	}
	return text
}

// degradedText 描述发现失败后的降级处置。
//
// 只有真的退回了显式模型才有这一句：发现失败且没有可退的模型时装配会整体失败，
// 那种情形由失败原因本身说清。
func degradedText(rec DiscoveryReport) string {
	if !rec.Degraded || rec.endpoint == nil {
		return ""
	}
	return fmt.Sprintf("退回显式模型 %d 个", len(rec.endpoint.Models))
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

// accessExtras 是主行从同一请求的上游尝试里带出的附加事实。
//
// 三者都只在「上游那一侧没有独立成行」时出现：尝试折叠进主行时，用量与上游 id 由主行承载；
// 尝试数大于一时，主行要交代这次请求打过几次上游，因为明细行只在真正有增量的请求上出现。
type accessExtras struct {
	usage *domain.Usage
	via   string
	tries int
}

// renderAccess 渲染一个块的主行：客户端侧终态，以及折叠进来的上游事实。
//
// 字段顺序按「排查时先看哪个」排：先是关联键，再是「谁问了什么」，然后结果与来源。
// 空值字段一律不出现，因此行内位置不是固定的——这是人读模式的固有代价，
// 需要固定字段位置时用 json 模式。
func renderAccess(clock string, record transport.AccessRecord, extras accessExtras, color bool) string {
	level := accessLevel(record.HTTPStatus)
	fields := []string{shortRequestID(record.RequestID), string(record.Protocol), record.Model}
	if record.Stream {
		fields = append(fields, "stream")
	}
	fields = append(fields,
		strconv.Itoa(record.HTTPStatus),
		durationText(time.Duration(record.DurationMS)*time.Millisecond),
	)
	if extras.tries > 1 {
		fields = append(fields, "tries "+strconv.Itoa(extras.tries))
	}
	if record.ErrorCode != "" {
		fields = append(fields, paintError(record.ErrorCode, color, level))
	}
	if extras.usage != nil {
		fields = append(fields, usageText(*extras.usage))
	}
	if extras.via != "" {
		fields = append(fields, "via "+extras.via)
	}
	// RemoteAddr 与 User-Agent 排在最后：它们最不重要却最长，
	// 放在中段会把结果字段挤到看不见的地方。
	fields = append(fields, record.RemoteAddr, record.UserAgent)
	return clock + " " + paintLevel(level, color) + kindColumn(kindAccess) + joinFields(fields...)
}

// upstreamLabel 把渠道标识与账号引用排成一段人读的定位文本。
//
// 单账号渠道不加账号：那时账号引用是空串，标签与账号池引入之前逐字一致。
// 主行的 via 与上游明细行共用它，两处因此不会各长出一种写账号的方式。
func upstreamLabel(rec domain.AttemptRecord) string {
	if rec.AccountRef == "" {
		return rec.UpstreamID
	}
	return rec.UpstreamID + " acct " + rec.AccountRef
}

// renderAttemptDetail 渲染一条上游明细。
//
// 它只写主行没有的事实：上游 id、两侧协议与模型的改写、上游用量、上游报文。
// 主行已经承载的结论不重抄——单次尝试时结果与耗时都与主行同一个事实，
// 重抄只会带来「同一个名字给了两个数」（入口耗时与上游耗时并不严格相等）。
//
// 单次尝试时结果字段也省：`ok` 对 2xx、`failed` 对 4xx/5xx 是同一次成功的两种说法。
// 只有两侧不一致（上游成功而客户端侧失败，或反过来）与取消才写出来。
func renderAttemptDetail(clock string, rec domain.AttemptRecord, multiple bool, status int, color bool) string {
	level := attemptLineLevel(rec, status)
	fields := []string{shortRequestID(rec.RequestID), "#" + strconv.Itoa(rec.Attempt), upstreamLabel(rec)}
	if rec.UpstreamProtocol != rec.ClientProtocol {
		fields = append(fields, fmt.Sprintf("%s→%s", rec.ClientProtocol, rec.UpstreamProtocol))
	}
	if rec.UpstreamModel != rec.RequestedModel {
		fields = append(fields, fmt.Sprintf("%s→%s", rec.RequestedModel, rec.UpstreamModel))
	}
	if outcome := attemptOutcomeText(rec.Outcome, status, multiple); outcome != "" {
		fields = append(fields, outcome)
	}
	if multiple {
		// 多次尝试时各次耗时不同，「哪一次慢」本身就是要看的东西，必须逐条写出。
		fields = append(fields, durationText(rec.EndedAt.Sub(rec.StartedAt)))
		if rec.ErrorCode != "" {
			fields = append(fields, paintError(rec.ErrorCode, color, level))
		}
	}
	if rec.Usage.Known() {
		fields = append(fields, usageText(rec.Usage))
	}
	if rec.ErrorDetail != "" {
		fields = append(fields, oneLineText(rec.ErrorDetail))
	}
	return clock + " " + paintLevel(level, color) + kindColumn(kindUpstream) + joinFields(fields...)
}

// attemptOutcomeText 决定这次尝试的结果要不要写进明细行。
//
// 返回空串表示主行已经说了同一件事。取消永远写：主行看不出「客户端走了」。
func attemptOutcomeText(outcome domain.AttemptOutcome, status int, multiple bool) string {
	if outcome == domain.AttemptCancelled || multiple {
		return string(outcome)
	}
	upstreamOK := outcome == domain.AttemptOK
	clientOK := status < 400
	if upstreamOK == clientOK {
		return ""
	}
	return string(outcome)
}

// renderSuppressed 渲染一条折叠汇总：同一类上游失败在一个窗口内被压掉了多少次。
//
// 上游持续限流时逐字相同的报文会连刷几十行，真正的信号「上游一直在限流」反而被淹没。
// 窗口内首条打全文，其余只计数，窗口结束时用这一行交代被压掉的量级。
func renderSuppressed(clock string, s suppressionView, color bool) string {
	fields := []string{
		"×" + strconv.Itoa(s.count),
		s.upstream,
		s.errorCode,
		"最近 " + durationText(s.span),
		"（同类上游失败已折叠）",
	}
	return clock + " " + paintLevel(s.level, color) + kindColumn(kindUpstream) + joinFields(fields...)
}

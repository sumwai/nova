package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/transport"
)

// logFormat 是生效的日志格式。
//
// 两种格式不是同一条记录的两种排版，而是两份不同的取舍：text 为人眼排版，会省略
// 「常见情况下不提供信息」的字段（同协议不写协议、同模型不写模型、零值子项不写），
// 并把同一请求的记录合为一块（见 log.go 的块组装），因此它不可逆，也不能拿来当数据源；
// json 保留全部字段并按语义分组，每个请求产出独立的记录，给采集器与 jq 用。
type logFormat int

const (
	logText logFormat = iota
	logJSON
)

// logger 是 nova 的输出口：启动横幅、配置提醒、访问日志与上游尝试日志都经由它。
//
// 它同时实现 transport.AccessLogger 与 domain.Observer，装配层因此把同一个对象交给
// 入口层与流水线，两类记录的时间基准与格式天然一致，不必两处各配一份。
//
// 并发安全靠一把互斥锁：人读模式下一个请求可能由多次 Write 组成（主行与明细行），
// 不加锁时两个并发请求的行会交错成读不通的字节流；锁同时保护暂存与折叠状态。
type logger struct {
	out    io.Writer
	level  slog.Level
	format logFormat
	color  bool
	mu     sync.Mutex

	// pending 暂存同一 request_id 的上游尝试，等访问日志到达时成块输出。
	// 只在 text 模式下存在：json 模式由下游按 request_id 关联，在网关内配对没有意义。
	//
	// 配对的前提是上游尝试恒早于访问日志到达（转发返回之后入口层才写访问日志），
	// 且同一个 request_id 只对应一个在飞请求。客户端自带的 request_id 若被并发复用，
	// 两次请求的尝试会落进同一槽并挂在先到的那个块下：记录不丢，但归属会错。
	// 那种输入下两条 access 主行本就同 id、无法区分，且 request_id 的生成权在客户端。
	pending map[string]*pendingAttempts
	// suppressed 记录被折叠的上游失败，键是抑制键。
	suppressed map[string]*suppression
	// window 是同类上游失败的折叠窗口。
	window time.Duration
	// clock 是取当前时刻的函数；测试用它把窗口边界定在确定的位置。
	clock func() time.Time
}

// pendingAttempts 是一个请求尚未与访问日志合并的上游尝试记录。
type pendingAttempts struct {
	attempts []domain.AttemptRecord
	first    time.Time
}

// suppression 是一类上游失败在当前窗口内的折叠计数。
type suppression struct {
	count     int
	first     time.Time
	last      time.Time
	level     slog.Level
	upstream  string
	errorCode string
}

// suppressionView 是渲染汇总行所需的那部分折叠状态。
type suppressionView struct {
	count     int
	span      time.Duration
	level     slog.Level
	upstream  string
	errorCode string
}

// view 冻结当前窗口的计数与跨度。
func (s *suppression) view(moment time.Time) suppressionView {
	return suppressionView{
		count:     s.count,
		span:      s.last.Sub(s.first),
		level:     s.level,
		upstream:  s.upstream,
		errorCode: s.errorCode,
	}
}

// suppressionKey 是折叠的归并键：同一条渠道上的同一个错误码算同一类失败。
//
// 成功的尝试不参与折叠，没有错误码的失败也无法判断是否同类。
func suppressionKey(rec domain.AttemptRecord) string {
	if rec.Outcome == domain.AttemptOK || rec.ErrorCode == "" {
		return ""
	}
	return rec.UpstreamID + "\x00" + rec.ErrorCode
}

// pendingLimit 是同时暂存的请求数上限。
//
// 正常路径上每条暂存记录都会在同一个请求的访问日志到达时被取走；上限只防御
// 「有尝试记录却没有访问日志」的异常情形，超限时把最旧的一条直接写出，宁可多写也不丢。
const pendingLimit = 256

// defaultSuppressionWindow 是同类上游失败的折叠窗口。
const defaultSuppressionWindow = 10 * time.Second

// 编译期断言：同一个输出口要同时满足入口层与流水线两边的观测接口。
var (
	_ transport.AccessLogger = (*logger)(nil)
	_ domain.Observer        = (*logger)(nil)
)

// newLogger 按配置建一个输出口。
//
// 级别与格式的取值在配置层已经校验过一次，这里仍然对未知取值报错而不是退回缺省：
// 配置层那次校验保护的是「读配置的人」，这次保护的是「装配层不悄悄改变行为」——
// 静默退回缺省会让一份写着 log_format xml 的配置看起来生效了。
func newLogger(cfg *config.Config, out io.Writer) (*logger, error) {
	level, err := parseLevel(cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	format, err := parseFormat(cfg.LogFormat)
	if err != nil {
		return nil, err
	}
	log := &logger{
		out:    out,
		level:  level,
		format: format,
		color:  detectColor(out),
		window: defaultSuppressionWindow,
		clock:  time.Now,
	}
	if format == logText {
		log.pending = make(map[string]*pendingAttempts)
		log.suppressed = make(map[string]*suppression)
	}
	return log, nil
}

// write 输出一条受级别过滤的记录。
func (l *logger) write(level slog.Level, line string, payload any) {
	l.emit(false, level, line, payload)
}

// writeAlways 输出一条不受级别过滤的记录。
//
// 启动横幅走这条路：它回答的是「这个进程现在用什么配置在跑」，与「哪些日志值得看」
// 是两件事。把它挂在 log_level 上，会让一个 log_level warn 的实例启动后完全看不出
// 自己监听在哪、配了几条渠道——而那些事实恰恰是排查的第一步。
func (l *logger) writeAlways(level slog.Level, line string, payload any) {
	l.emit(true, level, line, payload)
}

// emit 是唯一的输出口。
//
// line 是人读模式的完整文本，可以含换行（多行横幅因此也走这里）；payload 是 json 模式的
// 记录。时间与级别前缀由调用方拼进 line，因为「这条记录该不该带前缀」只有调用方知道——
// 多行横幅带上「INFO startup」毫无意义。
//
// 两个分支总是都拿到完整信息：让调用处判断「现在是什么格式」再决定要不要构造字段，
// 会把格式的判断散到每一处调用上。
func (l *logger) emit(always bool, level slog.Level, line string, payload any) {
	if l == nil {
		return
	}
	if !always && level < l.level {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emitLocked(line, payload)
}

// emitLocked 是持锁版本的唯一写出点。
func (l *logger) emitLocked(line string, payload any) {
	if l.format == logJSON {
		data, err := json.Marshal(payload)
		if err != nil {
			// 序列化失败不能让这条记录彻底消失：退化成一行自带说明的 JSON，
			// 至少留下「有一条日志没能写出来」这个事实。
			_, _ = fmt.Fprintf(l.out,
				"{\"level\":\"ERROR\",\"msg\":\"日志序列化失败\",\"error\":%q}\n", err.Error())
			return
		}
		_, _ = l.out.Write(append(data, '\n'))
		return
	}
	_, _ = io.WriteString(l.out, line+"\n")
}

// now 取当前时刻；时钟未注入时退回系统时钟。
func (l *logger) now() time.Time {
	if l.clock == nil {
		return time.Now()
	}
	return l.clock()
}

// startup 打出这份配置生效的事实，reloaded 只影响措辞。
//
// stats 是本次装配的目录事实：发现结果属于「这个进程在用哪份配置跑」这一回答，
// 与渠道数、对外模型数一起出现在横幅里。
func (l *logger) startup(cfg *config.Config, stats catalogStats, reloaded bool) {
	if l == nil {
		return
	}
	l.writeAlways(slog.LevelInfo,
		renderStartup(cfg, stats, reloaded),
		startupPayload(cfg, stats, reloaded))
}

// discovery 记一条端点的发现结果。
//
// 成功记 info，失败记 error：发现失败的后果是这条端点少了（或完全没有）可路由的模型，
// 不属于提示级别的事。清单自述还有下一页时另加一句，因为缺失的模型在客户端看来就是不存在。
func (l *logger) discovery(rec DiscoveryReport) {
	if l == nil {
		return
	}
	l.write(discoveryLevel(rec), renderDiscovery(clockAt(l.now()), rec, l.color), discoveryPayload(rec))
}

// discoveryLevel 是发现记录的级别。
//
// 失败记 error：后果是这条端点少了（或完全没有）可路由的模型。
// 成功但清单自述还有下一页时记 warn：缺失的模型在客户端看来就是不存在，
// 而成功本身的结论不需要占一个 warn，两者因此分得开。
func discoveryLevel(rec DiscoveryReport) slog.Level {
	switch {
	case rec.Err != nil:
		return slog.LevelError
	case rec.HasMore:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// warning 打出一条配置提醒。
func (l *logger) warning(warn config.Warning) {
	if l == nil {
		return
	}
	l.write(slog.LevelWarn, renderWarning(warn, l.color), warningPayload(warn))
}

// LogAccess 写一条请求级访问日志，实现 transport.AccessLogger。
//
// text 模式下它是整个块的收尾：同一请求暂存的上游尝试在这一刻与主行一起渲染，
// 因此上游尝试不必自己承担「这条记录属于哪次请求」的全部交代。
func (l *logger) LogAccess(record transport.AccessRecord) {
	if l == nil {
		return
	}
	if l.format == logJSON {
		l.emit(false, accessLevel(record.HTTPStatus), "", accessPayload(record))
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeBlockLocked(record)
}

// RecordAttempt 写一条上游尝试日志，实现 domain.Observer。
//
// 恒返回 nil：观测失败不得影响转发结果，而写一条日志除了目标不可写之外没有别的失败模式，
// 那种情况下把错误抛回流水线，只会让一个本该成功的请求因为日志而失败。
func (l *logger) RecordAttempt(_ context.Context, rec domain.AttemptRecord) error {
	if l == nil {
		return nil
	}
	if l.format == logJSON {
		l.emit(false, attemptLevel(rec.Outcome), "", attemptPayload(rec))
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stashAttemptLocked(rec)
	return nil
}

// writeBlockLocked 把一个请求的访问日志与暂存的上游尝试渲染成一个块。
//
// 顺序固定为主行在前、明细在后：主行是这次请求的结论，明细是结论背后的上游事实。
// 上游尝试本就先于访问日志产生（转发返回之后入口层才写访问日志），这个顺序不需要
// 额外排序就能成立，缓冲只用来把两者凑进同一次输出。
func (l *logger) writeBlockLocked(record transport.AccessRecord) {
	moment := l.now()
	l.flushExpiredLocked(moment)

	var pending *pendingAttempts
	if record.RequestID != "" {
		pending = l.pending[record.RequestID]
		delete(l.pending, record.RequestID)
	}

	visible := make([]domain.AttemptRecord, 0, 1)
	if pending != nil {
		for _, rec := range pending.attempts {
			if attemptLineLevel(rec, record.HTTPStatus) >= l.level {
				visible = append(visible, rec)
			}
		}
	}
	multiple := len(visible) > 1
	folded := len(visible) == 1 && canFoldAttempt(visible[0])
	clock := clockAt(moment)

	var b strings.Builder
	if accessLevel(record.HTTPStatus) >= l.level {
		b.WriteString(renderAccess(clock, record, accessExtrasFor(pending, visible, folded), l.color))
		b.WriteByte('\n')
	}
	if !folded {
		for _, rec := range visible {
			if l.suppressLocked(&b, clock, moment, rec, record.HTTPStatus) {
				continue
			}
			b.WriteString(renderAttemptDetail(clock, rec, multiple, record.HTTPStatus, l.color))
			b.WriteByte('\n')
		}
	}
	if b.Len() > 0 {
		_, _ = io.WriteString(l.out, b.String())
	}
}

// accessExtrasFor 决定主行要承载哪些上游事实。
//
// 用量与上游 id 只在唯一一次尝试被折叠进主行时出现：明细行已经写了它们的时候再往主行
// 抄一遍，就是把刚去掉的重复写回来。尝试数大于一时主行要交代总数，因为明细行只覆盖
// 达到级别的那些尝试，而被过滤掉的尝试同样发生过。
func accessExtrasFor(pending *pendingAttempts, visible []domain.AttemptRecord, folded bool) accessExtras {
	var extras accessExtras
	if pending != nil {
		extras.tries = len(pending.attempts)
	}
	if !folded {
		return extras
	}
	if usage := visible[0].Usage; usage.Known() {
		extras.usage = &usage
	}
	extras.via = visible[0].UpstreamID
	return extras
}

// stashAttemptLocked 暂存一条上游尝试，等同一请求的访问日志到达。
//
// 没有关联键的尝试无法与任何访问日志配对：直接按明细行输出，宁可失去成块排版，
// 也不把这条记录丢掉。
func (l *logger) stashAttemptLocked(rec domain.AttemptRecord) {
	if rec.RequestID == "" {
		_, _ = io.WriteString(l.out,
			renderAttemptDetail(clockAt(l.now()), rec, false, 0, l.color)+"\n")
		return
	}
	pending := l.pending[rec.RequestID]
	if pending == nil {
		pending = &pendingAttempts{first: l.now()}
		l.pending[rec.RequestID] = pending
	}
	pending.attempts = append(pending.attempts, rec)
	l.evictPendingLocked()
}

// evictPendingLocked 在暂存数超限时把最旧的一条直接写出。
//
// 这是防御「有尝试记录却没有访问日志」的异常路径（进程在请求中途退出、观测器被别处调用），
// 正常路径不会触发。丢掉最旧的而不是最新的，是为了让仍在进行中的请求有机会保持完整。
func (l *logger) evictPendingLocked() {
	if len(l.pending) <= pendingLimit {
		return
	}
	var oldestKey string
	var oldest *pendingAttempts
	for key, pending := range l.pending {
		if oldest == nil || pending.first.Before(oldest.first) {
			oldestKey, oldest = key, pending
		}
	}
	delete(l.pending, oldestKey)
	clock := clockAt(oldest.first)
	multiple := len(oldest.attempts) > 1
	for _, rec := range oldest.attempts {
		if attemptLineLevel(rec, 0) < l.level {
			continue
		}
		_, _ = io.WriteString(l.out,
			renderAttemptDetail(clock, rec, multiple, 0, l.color)+"\n")
	}
}

// suppressLocked 处理一条上游明细的折叠。
//
// 返回真表示这条明细不再单独渲染：要么已被计入当前窗口，要么已作为新窗口的首条写出。
// 窗口过期时先补一条汇总再开新窗口——汇总的数据只存在于折叠状态里，没有第二个地方
// 能事后补出来。
func (l *logger) suppressLocked(
	b *strings.Builder,
	clock string,
	moment time.Time,
	rec domain.AttemptRecord,
	status int,
) bool {
	key := suppressionKey(rec)
	if key == "" {
		return false
	}
	s := l.suppressed[key]
	if s == nil || moment.Sub(s.first) >= l.window {
		if s != nil {
			b.WriteString(renderSuppressed(clock, s.view(moment), l.color))
			b.WriteByte('\n')
		}
		l.suppressed[key] = &suppression{
			first:     moment,
			last:      moment,
			level:     attemptLineLevel(rec, status),
			upstream:  rec.UpstreamID,
			errorCode: rec.ErrorCode,
		}
		return false
	}
	s.count++
	s.last = moment
	return true
}

// flushExpiredLocked 补出已过期窗口的汇总。
//
// 不起定时器：下一次有日志要写时顺手看一眼。持续限流时新条目自己会触发补齐，
// 限流停下来之后，下一个请求的访问日志也会把最后一批折叠交代清楚。
func (l *logger) flushExpiredLocked(moment time.Time) {
	if len(l.suppressed) == 0 {
		return
	}
	clock := clockAt(moment)
	for key, s := range l.suppressed {
		if moment.Sub(s.first) < l.window {
			continue
		}
		if s.count > 0 {
			_, _ = io.WriteString(l.out, renderSuppressed(clock, s.view(moment), l.color)+"\n")
		}
		delete(l.suppressed, key)
	}
}

// reloadApplied 记录一次由管理端点触发的重载已经生效。
//
// 它与紧随其后的启动横幅各记一笔，两笔说的不是同一件事：横幅说的是「现在生效的是
// 什么配置」，这一条说的是「有人通过管理端点换了一次配置」。两条都在，才能把
// 「有人手动换了配置」与「进程重启了」分开。
func (l *logger) reloadApplied(path string) {
	if l == nil {
		return
	}
	line := clockNow() + " " + paintLevel(slog.LevelInfo, l.color) + kindColumn(kindReload) + path
	l.write(slog.LevelInfo, line, reloadPayload(path))
}

// failure 打出一条与配置无关的运行期警告，比如「旧装配释放失败」。
//
// 它不走配置提醒那条路：提醒带着配置文件里的行号与列号，指向一行写下的指令；
// 而运行期警告没有对应的那一行，硬套提醒的格式会让人去配置里找一处并不存在的错误。
func (l *logger) failure(message string, err error) {
	if l == nil {
		return
	}
	line := clockNow() + " " + paintLevel(slog.LevelWarn, l.color) + kindColumn(kindWarning) + message
	if err != nil {
		line += "：" + err.Error()
	}
	l.write(slog.LevelWarn, line, failurePayload(message, err))
}

// attemptLineLevel 是一条上游明细的级别。
//
// 取「尝试自身的级别」与「主行级别」中的较高者：明细行属于主行那次请求，主行是 error
// 而明细压成 warn，会让按级别过滤的人只拿到半截信息；反过来，200 请求里失败过的尝试
// 仍要按 warn 输出，否则「重试过一次并且失败过」这条事实会随主行的 info 一起消失。
//
// 取消是例外：它记 debug 本就是为了不在长流场景里刷屏，跟主行走会把这条取舍作废。
func attemptLineLevel(rec domain.AttemptRecord, status int) slog.Level {
	level := attemptLevel(rec.Outcome)
	if level <= slog.LevelDebug {
		return level
	}
	if main := accessLevel(status); main > level {
		level = main
	}
	return level
}

// canFoldAttempt 报告一次成功的上游尝试是否已经被主行完整表达。
//
// 条件全部满足时它不单独占行：主行已经写了协议与模型（同协议同模型因此是前提），
// 用量与上游 id 由 accessExtrasFor 并入主行，而成功尝试没有要单独交代的上游报文。
func canFoldAttempt(rec domain.AttemptRecord) bool {
	return rec.Outcome == domain.AttemptOK &&
		rec.ErrorDetail == "" &&
		rec.UpstreamProtocol == rec.ClientProtocol &&
		rec.UpstreamModel == rec.RequestedModel
}

// accessLevel 按 HTTP 状态码决定访问日志级别。
//
// 分档边界直接用 net/http 的常量：它们是这套分档的语义来源，写成裸数字既看不出含义，
// 也会与标准库的取值脱钩。
func accessLevel(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// attemptLevel 按尝试结果决定上游尝试日志的级别。
//
// 取消记 debug 是有意的：客户端主动断开是长流场景下的常见结局，把它记成 warn，
// 会把真正的上游故障淹没在一片「客户端走了」里。
func attemptLevel(outcome domain.AttemptOutcome) slog.Level {
	switch outcome {
	case domain.AttemptFailed:
		return slog.LevelWarn
	case domain.AttemptCancelled:
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

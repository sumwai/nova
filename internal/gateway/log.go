package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/transport"
)

// logFormat 是生效的日志格式。
//
// 两种格式不是同一条记录的两种排版，而是两份不同的取舍：text 为人眼排版，会省略
// 「常见情况下不提供信息」的字段（同协议不写协议、同模型不写模型、零值子项不写），
// 因此它不可逆，也不能拿来当数据源；json 保留全部字段并按语义分组，给采集器与 jq 用。
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
// 并发安全靠一把互斥锁：一条记录可能由多次 Write 组成（人读模式下失败尝试的上游明细
// 另起一行），不加锁时两条并发日志会交错成读不通的字节流。
type logger struct {
	out    io.Writer
	level  slog.Level
	format logFormat
	color  bool
	mu     sync.Mutex
}

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
	return &logger{out: out, level: level, format: format, color: detectColor(out)}, nil
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

// startup 打出这份配置生效的事实，reloaded 只影响措辞。
func (l *logger) startup(cfg *config.Config, reloaded bool) {
	if l == nil {
		return
	}
	l.writeAlways(slog.LevelInfo, renderStartup(cfg, reloaded), startupPayload(cfg, reloaded))
}

// warning 打出一条配置提醒。
func (l *logger) warning(warn config.Warning) {
	if l == nil {
		return
	}
	l.write(slog.LevelWarn, renderWarning(warn, l.color), warningPayload(warn))
}

// LogAccess 写一条请求级访问日志，实现 transport.AccessLogger。
func (l *logger) LogAccess(record transport.AccessRecord) {
	if l == nil {
		return
	}
	l.write(accessLevel(record.HTTPStatus), renderAccess(record, l.color), accessPayload(record))
}

// RecordAttempt 写一条上游尝试日志，实现 domain.Observer。
//
// 恒返回 nil：观测失败不得影响转发结果，而写一条日志除了目标不可写之外没有别的失败模式，
// 那种情况下把错误抛回流水线，只会让一个本该成功的请求因为日志而失败。
func (l *logger) RecordAttempt(_ context.Context, rec domain.AttemptRecord) error {
	if l == nil {
		return nil
	}
	l.write(attemptLevel(rec.Outcome), renderAttempt(rec, l.color), attemptPayload(rec))
	return nil
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
	line := clockNow() + " " + paintLevel(slog.LevelInfo, l.color) + "  reload  " + path
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
	line := clockNow() + " " + paintLevel(slog.LevelWarn, l.color) + "  " + message
	if err != nil {
		line += "：" + err.Error()
	}
	l.write(slog.LevelWarn, line, failurePayload(message, err))
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

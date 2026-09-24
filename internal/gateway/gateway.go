// Package gateway 把一份配置装配成可服务的运行时对象，并管理监听、
// 配置热重载与优雅退出。
//
// 它与 config 包的分工是「说什么」与「做什么」：config 只描述配置说了什么，
// gateway 才把它变成会占用端口、持有连接的东西。这条界线让 `config check`
// 可以在不监听、不连接的前提下完整校验一份配置。
package gateway

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/sumwai/nova/internal/config"
)

// Assembly 是一次配置装配的产物。
//
// 一次 reload 造一份新的 Assembly 整体换入，而不是就地改字段：换入是一次指针
// 赋值，读侧取一次指针就拿到自洽的一整套；就地改字段则会出现「新的数据面配着
// 旧的日志句柄」这类半旧半新的状态，而这种状态只在并发下才暴露。
type Assembly struct {
	Config  *config.Config
	Logger  *slog.Logger
	Handler http.Handler
}

// Close 释放这次装配独占的资源。
//
// 本版没有任何需要显式关闭的东西，因此它是空操作。保留这个方法是给「换出旧装配」
// 一个明确的收口点：转发链路接进来之后，上游连接池与空闲连接都在这里释放，
// 而调用方不必为此改一行。
//
// 对 nil 接收者返回 nil：调用方在「没有旧装配」这个边界上不该还要先判空。
func (a *Assembly) Close() error {
	return nil
}

// Holder 持有当前生效的装配，支持整体原子换入。
//
// 用 RWMutex 而不是 atomic.Pointer：换入要与「判定可否换入」组成一个临界区，
// 而裸指针原子写没有地方安放那一步判定。
type Holder struct {
	mu  sync.RWMutex
	cur *Assembly
}

// NewHolder 用一个非空装配建持有者。
//
// 不接受 nil：每个请求的入口都要读当前装配，若它可能是 nil，那次读取就得
// 先判空——于是「启动时装配失败」会退化成每次请求上的一次空指针检查。
func NewHolder(first *Assembly) *Holder {
	if first == nil {
		panic("gateway: NewHolder 不接受 nil 装配")
	}
	return &Holder{cur: first}
}

// Current 返回当前生效的装配，一定非 nil。
func (h *Holder) Current() *Assembly {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cur
}

// replace 整体换入一份新装配，返回被换出的旧装配。
//
// 只换入、不关闭：关闭要放在换出之后由调用方执行。顺序反了就会出现一段
// 「旧资源已关、新装配尚未生效」的窗口，而这段窗口里的请求全部失败。
func (h *Holder) replace(next *Assembly) *Assembly {
	h.mu.Lock()
	defer h.mu.Unlock()
	previous := h.cur
	h.cur = next
	return previous
}

// ServeHTTP 把请求交给当前装配的数据面。
//
// 只在对齐指针时持读锁，整个请求处理期间不再持锁：流式响应可能持续几分钟，
// 持锁会让这期间的每一次 reload 都被挡在门外。
func (h *Holder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Current().Handler.ServeHTTP(w, r)
}

// Assemble 按配置装配出一套运行时对象。
//
// 所有装配缺陷都在这里报出，而不是等第一个请求打进来：配置错误应该在启动与
// reload 时以退出码和失败回应被看见。拖到运行期，它就变成某个请求的 500，
// 而那时已经没有人记得自己刚改过配置。
func Assemble(cfg *config.Config, logOutput io.Writer) (*Assembly, error) {
	logger, err := newLogger(cfg, logOutput)
	if err != nil {
		return nil, err
	}
	return &Assembly{
		Config:  cfg,
		Logger:  logger,
		Handler: newDataPlane(),
	}, nil
}

// newLogger 按配置里的日志级别建一个 slog 句柄。
//
// 级别在装配期就判定，非法取值当场报错：退化成缺省级别会让「我明明写了 debug
// 却没有 debug 日志」变成一桩要靠读解析器源码才能破的悬案。
func newLogger(cfg *config.Config, out io.Writer) (*slog.Logger, error) {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("日志级别 %q 不认识，取值只能是 debug / info / warn / error", cfg.LogLevel)
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})), nil
}

// newDataPlane 造数据面的 HTTP 处理器。
//
// 本版只实现存活探针，其余路径固定回 501 而不是 404：对调用方来说「路径对、
// 功能还没做」与「路径本身不存在」是两件事，混成 404 会让人反复检查自己拼的
// URL，而真相是这一版还没有转发能力。
func newDataPlane() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "转发链路尚未实现", http.StatusNotImplemented)
	})
	return mux
}

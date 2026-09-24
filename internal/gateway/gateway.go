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
	"github.com/sumwai/nova/internal/credential"
	"github.com/sumwai/nova/internal/pipeline"
	"github.com/sumwai/nova/internal/transport"
	"github.com/sumwai/nova/internal/upstream"
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

	// clients 是这次装配独占的 HTTP 客户端。换出时逐个关掉它们的空闲连接：
	// 不关的话，每次 reload 都会把上一份配置的连接池留到空闲超时才释放，
	// 而上游视角里那些连接仍然开着。
	clients []*http.Client
}

// Close 释放这次装配独占的资源，即上游 HTTP 客户端的空闲连接。
//
// 只关空闲连接，不打断在途请求：换出发生在「新装配已经生效」之后，此刻旧装配上
// 还可能挂着正在流式返回的请求，掐断它们会让客户端收到一个截断的 200。
//
// 对 nil 接收者返回 nil：调用方在「没有旧装配」这个边界上不该还要先判空。
func (a *Assembly) Close() error {
	if a == nil {
		return nil
	}
	for _, client := range a.clients {
		client.CloseIdleConnections()
	}
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

	// 三种协议的适配器做成单例：客户端侧按请求路径取，上游侧按路由协议取，
	// 两处用同一张表，因此「客户端能进来的协议」与「上游能解码的协议」不会漂移。
	adapters := newAdapters()
	lookup := upstreamAdapterLookup(adapters)

	// 凭据表按 provider 名字建：它就是 domain.Route.CredentialRef 的取值，
	// 两张表因此天然对齐。运行期由路由取出该用哪份密钥，注入形态由路由协议决定，
	// 所以同一份配置里 OpenAI 系端点与 Anthropic 端点可以各用各的头形态。
	credentials := credential.New(credentialsByRef(cfg.Providers))

	// 上游客户端持有自己的 HTTP 客户端：Assembly 要能在换出时关掉它的空闲连接。
	// 这里不设全局默认超时，每条候选都带着自己端点的 timeout（配置层已兜底 60s）。
	upstreamHTTP := &http.Client{}
	upstreamClient, err := upstream.New(upstream.Options{
		HTTPClient: upstreamHTTP,
		Headers:    credentials,
		Adapters:   lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("构造上游客户端失败：%w", err)
	}

	forwarder, err := pipeline.New(forwarderOptions(routesByModel(cfg), lookup, upstreamClient))
	if err != nil {
		return nil, fmt.Errorf("构造转发流水线失败：%w", err)
	}

	resolve := adapterResolver(adapters)
	forward, err := transport.New(transport.Options{
		Forwarder: forwarder,
		Adapters:  resolve,
	})
	if err != nil {
		return nil, fmt.Errorf("构造 HTTP 入口失败：%w", err)
	}

	return &Assembly{
		Config:  cfg,
		Logger:  logger,
		Handler: newDataPlane(forward, cfg, resolve),
		clients: []*http.Client{upstreamHTTP},
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

// healthzPath 是存活探针的路径。
const healthzPath = "/healthz"

// newDataPlane 造数据面的 HTTP 处理器。
//
// 转发入口挂在根路径上，由它自己按固定映射判定路径是否受支持，并给出协议化的 404；
// 健康检查另挂一条，不经鉴权也不经转发——探活只关心进程是否在线，让它依赖客户端凭据
// 会让编排系统在上游或凭据出问题时，重启一个本身健康的网关。
func newDataPlane(forward http.Handler, cfg *config.Config, resolve transport.AdapterResolver) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(healthzPath, healthz)
	mux.Handle("/", authorize(forward, cfg.ClientKeys, resolve))
	return mux
}

// healthz 是存活探针：任何方法都回 200 与纯文本 ok。
//
// 不在此处探活上游：那会让探活随上游抖动而失败，进而在编排系统里反复重启一个本身
// 健康的网关。
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok")
}

// Package gateway 把一份配置装配成可服务的运行时对象，并管理监听、
// 配置热重载与优雅退出。
//
// 它与 config 包的分工是「说什么」与「做什么」：config 只描述配置说了什么，
// gateway 才把它变成会占用端口、持有连接的东西。这条界线让 `config check`
// 可以在不监听、不连接的前提下完整校验一份配置。
package gateway

import (
	"context"
	"fmt"
	"io"
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
	Logger  *logger
	Handler http.Handler

	// Models 是本次装配的对外模型目录，按首现顺序去重。
	// 它是数据面 GET /v1/models 与 `nova models` 的唯一数据来源。
	Models []ModelEntry

	// Discoveries 是本次装配逐条发现型端点的结果，供 `nova models` 展示。
	Discoveries []DiscoveryReport

	// Stats 是本次装配的目录汇总，供启动横幅与 reload 日志使用。
	Stats catalogStats

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
//
// ctx 用于装配期的模型发现：启动路径传进程 context，reload 路径传管理端点请求的
// context（客户端断开即取消这次发现）。为 nil 时按无上限处理，供只做装配检查的调用方使用。
func Assemble(ctx context.Context, cfg *config.Config, logOutput io.Writer) (*Assembly, error) {
	logger, err := newLogger(cfg, logOutput)
	if err != nil {
		return nil, err
	}

	// 各协议的适配器做成单例：客户端侧按请求路径取，上游侧按路由协议取，
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

	// 模型发现排在装配的这一步：它决定这次装配的选路表，而它的失败处置要看端点
	// 有没有显式声明的模型。发现与转发共用同一个 HTTP 客户端，连接池只有一份。
	if ctx == nil {
		ctx = context.Background()
	}
	discovered, reports := runDiscovery(ctx, cfg, listingFetcher{
		http:        upstreamHTTP,
		credentials: credentials,
		adapters:    adapters,
	})
	for i := range reports {
		// 有显式声明的模型可选时才算降级；没有可退的模型时装配随后整体失败，
		// 那种情形不由「已降级」描述。
		if reports[i].Err != nil && len(reports[i].endpoint.Models) > 0 {
			reports[i].Degraded = true
		}
		logger.discovery(reports[i])
		// 没命中的改名规则单独提一句：它多半意味着模式写错，
		// 或者被 allow / deny 先拦掉了——那种情况下改名根本轮不到执行。
		for _, alias := range reports[i].UnusedAliases {
			logger.warning(config.Warning{
				File: alias.File,
				Line: alias.Line,
				Msg: fmt.Sprintf("expose %s %s 没有命中清单里的任何模型；"+
					"检查上游 id 是否与模式一致（被 allow / deny 拦下的项不会走到改名）",
					alias.From, alias.To),
			})
		}
	}
	if err := fatalDiscovery(reports); err != nil {
		return nil, err
	}

	endpoints := assembleEndpoints(cfg, discovered)
	models := modelEntries(endpoints, discovered)
	stats := catalogStatsFrom(reports, models)

	upstreamClient, err := upstream.New(upstream.Options{
		HTTPClient: upstreamHTTP,
		Headers:    credentials,
		Adapters:   lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("构造上游客户端失败：%w", err)
	}

	resolver, routeWarnings := newModelRouteResolver(endpoints, cfg)
	for _, warning := range routeWarnings {
		logger.warning(warning)
	}
	forwarder, err := pipeline.New(forwarderOptions(resolver, lookup, upstreamClient, logger))
	if err != nil {
		return nil, fmt.Errorf("构造转发流水线失败：%w", err)
	}

	resolve := adapterResolver(adapters)
	forward, err := transport.New(transport.Options{
		Forwarder: forwarder,
		Adapters:  resolve,
		// 访问日志与上游尝试日志用同一个输出口：两类记录靠 request_id 关联，
		// 格式与时间基准因此天然一致，不必在排查时对两条不同风格的日志。
		Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("构造 HTTP 入口失败：%w", err)
	}

	return &Assembly{
		Config:      cfg,
		Logger:      logger,
		Handler:     newDataPlane(forward, cfg, resolve, models),
		Models:      models,
		Discoveries: reports,
		Stats:       stats,
		clients:     []*http.Client{upstreamHTTP},
	}, nil
}

// healthzPath 是存活探针的路径。
const healthzPath = "/healthz"

// newDataPlane 造数据面的 HTTP 处理器。
//
// 转发入口挂在根路径上，由它自己按固定映射判定路径是否受支持，并给出协议化的 404；
// 模型清单另挂一条精确路径，同样过鉴权，不带凭据时按协议形状回 401；
// 健康检查也另挂一条，不经鉴权也不经转发——探活只关心进程是否在线，让它依赖客户端凭据
// 会让编排系统在上游或凭据出问题时，重启一个本身健康的网关。
func newDataPlane(
	forward http.Handler,
	cfg *config.Config,
	resolve transport.AdapterResolver,
	models []ModelEntry,
) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(healthzPath, healthz)
	// 精确路径优先于 "/"：模型清单因此不会落到转发入口上被当成未注册路径。
	mux.Handle(modelsPath, authorize(newModelsHandler(models), cfg.ClientKeys, resolve))
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

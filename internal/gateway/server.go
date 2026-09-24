package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/sumwai/nova/internal/config"
)

// 两个监听器的读头超时，与优雅退出的等待上限。
//
// readHeaderTimeout 防的是慢速发头的连接占住一个 goroutine 不放（Slowloris）。
// shutdownTimeout 是给在途请求的收尾时间：设上限是为了不让一个卡住的请求
// 把进程永远吊在退出流程里。
const (
	readHeaderTimeout = 30 * time.Second
	shutdownTimeout   = 30 * time.Second
)

// Options 是 Run 的入参。
type Options struct {
	// ConfigPath 是启动时读取的配置文件路径。
	// reload 请求会带来它自己的路径，因此这个字段只决定启动读哪一份。
	ConfigPath string

	// LogOutput 是横幅、日志与提醒的落点。
	// 它与命令行结果输出分开：日志走 stderr 时，`nova version --json | jq`
	// 才不会被日志行污染。
	LogOutput io.Writer
}

// Run 装配配置并开始服务，直到 ctx 结束或某个监听器失败。
//
// 它不自己建 ctx、也不自己接信号：信号是「进程层面的调用方」才有资格决定的事，
// 而 Run 只需要知道「什么时候该停」。这让同一段服务逻辑能被测试用
// context.WithCancel 驱动，不必给测试进程发真的信号。
func Run(ctx context.Context, opt Options) error {
	cfg, err := config.Load(opt.ConfigPath)
	if err != nil {
		return err
	}
	first, err := Assemble(cfg, opt.LogOutput)
	if err != nil {
		return err
	}

	s := &server{holder: NewHolder(first), out: opt.LogOutput}
	reportWarnings(s.out, cfg)
	banner(s.out, cfg, false)
	return s.serve(ctx)
}

// server 是一次 Run 的进程级状态。
//
// 它持有的都是「跨 reload 不变」的东西：装配被换入 Holder，而监听器与输出目标
// 属于这次进程，不属于某一份配置。
type server struct {
	holder *Holder
	out    io.Writer
}

// serve 建立两个监听器并等它们结束。
//
// 两个地址是两份独立的事实：数据面失败说明网关无法服务；管理端点失败说明 reload
// 会静默失效。后者同样不可容忍——一个连不上的管理端点会让「配置改了却没生效」
// 变成一桩无从下手的悬案，因此任一监听器报错都终止整个进程。
func (s *server) serve(ctx context.Context) error {
	cfg := s.holder.Current().Config

	gatewayLn, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("监听数据面 %s 失败：%w", cfg.Listen, err)
	}
	adminLn, err := net.Listen("tcp", cfg.Admin)
	if err != nil {
		// 数据面已经起在端口上，管理端点失败时若不关掉它，
		// 这次失败的启动会在端口上留下一个连不上的监听器。
		_ = gatewayLn.Close()
		return fmt.Errorf("监听管理端点 %s 失败：%w", cfg.Admin, err)
	}

	// Addr 只用于日志与关闭时的报错文本：真正生效的地址来自上面两个 listener，
	// 它们是先 listen 再 serve 的，因此「端口被占用」在这里就已经有答案。
	gatewaySrv := &http.Server{
		Addr:              gatewayLn.Addr().String(),
		Handler:           s.holder,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	adminSrv := &http.Server{
		Addr:              adminLn.Addr().String(),
		Handler:           newAdminHandler(s.holder, s.reload),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	// 缓冲 2：两个 goroutine 各写一次。不缓冲时总有一个可能永远阻塞在发送上，
	// 于是进程退出时留下一个卡住的 goroutine。
	errs := make(chan error, 2)
	go func() { errs <- serveHTTP(gatewaySrv, gatewayLn) }()
	go func() { errs <- serveHTTP(adminSrv, adminLn) }()

	select {
	case <-ctx.Done():
		// 停止的可见性由调用方负责：只有它知道这次停止来自信号、来自测试收尾，
		// 还是别的什么。在这里再打一条只会与调用方那条重复。
		return shutdownAll(gatewaySrv, adminSrv)

	case err := <-errs:
		// 任一监听器退出就收掉另一个：只剩管理端点活着时，reload 会成功，
		// 但没有任何请求能被服务——那比进程直接退出更难察觉。
		_ = gatewaySrv.Close()
		_ = adminSrv.Close()
		return err
	}
}

// serveHTTP 在一个已建好的 listener 上服务，把「正常关闭」与「真失败」分开。
//
// http.ErrServerClosed 是 Shutdown 与 Close 的正常副作用。把它当错误返回，
// 每一次正常停止都会打出一条失败日志，日志里的噪声就是这样攒起来的。
func serveHTTP(srv *http.Server, ln net.Listener) error {
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// shutdownAll 停止接收新请求，并给在途请求留出收尾时间。
//
// 用 Shutdown 而不是 Close：Close 会立刻掐断在途请求，包括正在流式返回的响应，
// 客户端看到的是一个半截的 200——它比一个明确的错误更难排查。
//
// 两个服务共用一个带超时的 ctx，而不是各给一个超时：整体退出时间因此有上界，
// 不会随监听器个数线性增长。
func shutdownAll(servers ...*http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	var errs []error
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("关闭 %s 失败：%w", srv.Addr, err))
		}
	}
	return errors.Join(errs...)
}

// reload 按一份新配置重新装配并整体换入。
//
// 顺序是「先完整装配、再换入」：装配失败时旧装配原样继续服务，现场不变。
// 反过来先换入再装配，就会出现一段「新配置已生效、但还没装配完」的空窗。
//
// ctx 目前只用于提前放弃，装配本身还不做网络 IO；转发链路接进来之后，
// 启动期的模型发现会需要它，那时这个参数就是现成的。
func (s *server) reload(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	next, err := config.Load(path)
	if err != nil {
		return err
	}
	if err := checkReloadable(s.holder.Current().Config, next); err != nil {
		return err
	}
	assembled, err := Assemble(next, s.out)
	if err != nil {
		return err
	}

	previous := s.holder.replace(assembled)
	reportWarnings(s.out, next)
	banner(s.out, next, true)

	if err := previous.Close(); err != nil {
		// 旧装配释放失败不影响「新装配已经生效」这一事实，因此记一条警告，
		// 而不是把这次 reload 判成失败——那会让人以为新配置没生效而去改它。
		assembled.Logger.Warn("旧装配释放失败", "error", err)
	}
	return nil
}

// checkReloadable 拒绝那些 reload 兑现不了的改动。
//
// 监听地址是 http.Server 的构造期事实，改它必须重建 listener，而重建过程中
// 必然有一段「旧的已关、新的未起」。与其静默沿用旧地址——那会让配置、横幅、
// 日志看起来全都对了，请求却仍打在旧端口上——不如明确要求重启。
func checkReloadable(old, next *config.Config) error {
	var changed []string
	if old.Listen != next.Listen {
		changed = append(changed, fmt.Sprintf("listen: %s → %s", old.Listen, next.Listen))
	}
	if old.Admin != next.Admin {
		changed = append(changed, fmt.Sprintf("admin: %s → %s", old.Admin, next.Admin))
	}
	if len(changed) == 0 {
		return nil
	}
	return fmt.Errorf("以下改动需要重启 nova，reload 不接受：\n  %s", strings.Join(changed, "\n  "))
}

// reportWarnings 把解析期攒下的提醒一次输出完。
//
// 一次说完而不是报一条改一条：正在改配置的人被逐条中断，会把一次编辑拆成
// 好几轮往返，而每条提醒本来就没有先后依赖。
func reportWarnings(w io.Writer, cfg *config.Config) {
	for _, warn := range cfg.Warnings {
		_, _ = fmt.Fprintf(w, "提醒 %s\n", warn.String())
	}
}

// banner 打出这份配置生效的事实。
//
// 启动时与每次成功的 reload 后各打一遍：日志里因此可以按横幅切分「哪一段属于
// 哪份配置」，而不必靠时间戳去猜某条日志是在哪次重载之后产生的。
// reloaded 只影响首行措辞，让「刚启动」与「刚重载」在读日志时一眼可分。
func banner(w io.Writer, cfg *config.Config, reloaded bool) {
	headline := "nova 已启动"
	if reloaded {
		headline = "nova 已重载"
	}
	_, _ = fmt.Fprintf(w, "%s\n", headline)
	_, _ = fmt.Fprintf(w, "  配置文件  %s\n", cfg.Path)
	_, _ = fmt.Fprintf(w, "  配置代数  %d\n", cfg.Schema)
	_, _ = fmt.Fprintf(w, "  监听      %s\n", cfg.Listen)
	_, _ = fmt.Fprintf(w, "  管理端点  %s\n", cfg.Admin)
	_, _ = fmt.Fprintf(w, "  日志级别  %s\n", cfg.LogLevel)
}

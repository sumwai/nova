package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// AdminPath 是管理端点唯一接受的路径。
//
// 导出它是为了让命令行侧不必复制一份字面量：重载命令与服务端必须指向同一条路径，
// 各写一遍时改漏一处，现象会是「reload 报成功但网关没反应」或者干脆 404——
// 两者都要翻源码才能定位。
const AdminPath = "/load"

// reloadPathLimit 是管理端点请求体的字节上限。
//
// 请求体只承载一个配置文件路径，用不到更长。设上限是为了不让一个不设防的
// 本地接口成为内存放大器。
const reloadPathLimit = 4096

// adminHandler 是管理端点的 HTTP 处理器。
type adminHandler struct {
	holder *Holder
	reload func(ctx context.Context, path string) error

	// mu 串行化重载。后到的请求等前一个做完，不做合并也不排队：
	// 两次装配并发跑会同时读文件、同时连上游，而它们换入的先后无法由
	// 请求到达顺序决定，最终生效的是哪一份就成了竞态。
	mu sync.Mutex
}

// newAdminHandler 造管理端点处理器。
//
// reload 以函数而非接口注入，是为了让这个处理器不依赖 server 的具体形态：
// 它需要的只是「拿一个路径换一份生效的配置」，而不是整个服务对象。
func newAdminHandler(holder *Holder, reload func(context.Context, string) error) http.Handler {
	return &adminHandler{holder: holder, reload: reload}
}

// ServeHTTP 实现 POST /load：请求体是配置文件路径，成功即 200 空体。
//
// 请求体传路径而不是配置内容，是为了让服务端走与启动期完全同一个 config.Load。
// 若改成「把内容传过来」，调用方与服务端就各解析一遍，两份理解迟早分叉，
// 而分叉的那一刻表现为「reload 报成功、行为却没变」——最难查的一类故障。
func (h *adminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != AdminPath {
		http.Error(w, "未知管理路径", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "管理端点只接受 POST", http.StatusMethodNotAllowed)
		return
	}

	path, err := readReloadPath(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// 日志句柄在每次请求时从当前装配取：reload 会换掉日志级别，
	// 从而换掉整个句柄，缓存一个回来用会让新级别要到下次重启才生效。
	logger := h.holder.Current().Logger

	if err := h.reload(r.Context(), path); err != nil {
		logger.Error("reload_failed", "path", path, "error", err)
		// 失败一律 500，响应体就是错误文本本身：错误里已经带了 文件:行:列，
		// 再包一层状态码语义只会让调用方多一次翻译。
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logger.Info("reloaded", "path", path)
	w.WriteHeader(http.StatusOK)
}

// readReloadPath 从请求体读出配置文件路径。
//
// 多读一个字节是为了区分「恰好读满」与「超限」：只读上限个字节的话，超长输入
// 会被静默截断成一个看起来合法的短路径，然后去重载一个别的文件——
// 这种错误不会报错，只会让人对着「我明明传的是 A」百思不解。
func readReloadPath(r *http.Request) (string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, reloadPathLimit+1))
	if err != nil {
		return "", fmt.Errorf("读取请求体失败：%w", err)
	}
	if len(body) > reloadPathLimit {
		return "", fmt.Errorf("配置文件路径超过 %d 字节", reloadPathLimit)
	}
	path := strings.TrimSpace(string(body))
	if path == "" {
		return "", errors.New("请求体里没有配置文件路径")
	}
	return path, nil
}

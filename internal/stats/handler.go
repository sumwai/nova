package stats

import (
	"encoding/json"
	"net/http"
)

// NewHandler 造统计查询的 HTTP 处理器。
//
// 处理器只读 Store，不修改任何采集状态：查询是纯读，与写入路径共用同一把读写锁。
func NewHandler(store *Store) http.Handler {
	return &handler{store: store}
}

type handler struct {
	store *Store
}

// ServeHTTP 处理 GET /debug/stats。
//
// 只接受 GET 与 HEAD：HEAD 在 net/http 里由同一个分支处理，写出会被框架丢弃，
// 因此不必单独实现，但也不能当成方法错误拒掉。
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
		writeJSONError(w, http.StatusMethodNotAllowed, "统计端点只接受 GET")
		return
	}

	query, err := parseQuery(r.URL.Query(), h.store.now())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	report, err := h.store.Report(query)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	body, err := renderReport(report, query.Pretty)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "编码统计结果失败")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 统计是时刻快照，缓存它没有任何意义，反而会让「刚发的请求怎么没算上」成为一个陷阱。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func renderReport(report Report, pretty bool) ([]byte, error) {
	if pretty {
		return json.MarshalIndent(report, "", "  ")
	}
	return json.Marshal(report)
}

// writeJSONError 用 JSON 回一个错误，使调用方不必为错误路径另写一套解析。
func writeJSONError(w http.ResponseWriter, status int, message string) {
	body, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

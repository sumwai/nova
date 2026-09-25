// Package stats 在进程内采集请求与用量事实，供 HTTP 查询端点做聚合。
//
// 它与日志的分工是「给人看的记录」与「可聚合的事实」：日志逐条输出、受级别与格式
// 影响、随装配整体换入换出；本包只保留一个进程级的窗口，把每条请求压成一份合并
// 记录，读取时再按查询条件聚合。两者消费同一对观测接口，因此看到的事实一致。
//
// 本包的定位是实时快照，不是归档：数据只活在内存里，进程重启即清空。需要跨重启的
// 历史时应走 log_format json 的下游管道，而不是把存储塞进转发进程。
package stats

import (
	"time"

	"github.com/sumwai/nova/internal/domain"
)

// Request 是一次客户端请求合并后的观测记录。
//
// 它由入口层的访问记录与流水线的上游尝试记录按 request_id 合并而来：两侧各自只持有
// 事实的一半——访问侧没有用量，尝试侧没有客户端事实——合起来才是「这次请求花了多少、
// 打到了哪里、客户端是谁」的完整答案。
//
// 本类型只在写入侧成型，读取侧一律从它派生，因此「窗口内的数字」只有一个来源，
// 不存在快路径与慢路径对不上的可能。
type Request struct {
	Time         time.Time
	RequestID    string
	Protocol     string
	Model        string
	Stream       bool
	Status       int
	DurationMS   int64
	WrittenBytes int
	ErrorCode    string
	RemoteAddr   string
	UserAgent    string
	// Client 是 UserAgent 归一化后的产品名。聚合维度一律用它：原始 User-Agent 无界，
	// 直接当聚合键会让内存随客户端版本号漂移而无上限增长。
	Client string
	// Providers 是本次请求尝试过的渠道名，去重后按首现顺序排列，供请求级的渠道过滤使用。
	// 一次请求发生回退时会跨多条渠道，因此它可能有多项。
	Providers []string
	Attempts  []AttemptSummary
	// Usage 是各次取得用量的尝试之和；一次都没取得时为来源未知的零值。
	Usage domain.Usage
	// UsageUnknown 是本次请求里未取得用量的尝试数。它与 Usage 全零是两件相反的事：
	// 前者是「上游没有给出计数」，后者可能是「计数确实为零」（见 domain.Usage.Known）。
	UsageUnknown int
}

// AttemptSummary 是一条上游尝试在统计里保留的事实。
//
// 它比 domain.AttemptRecord 窄：只留聚合与归因需要的字段，报文细节与改写标注不进来，
// 那些属于日志。
type AttemptSummary struct {
	Provider   string
	Upstream   string
	Account    string
	Outcome    string
	ErrorCode  string
	DurationMS int64
	Usage      domain.Usage
}

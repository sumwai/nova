package pipeline

// Breaker 是渠道熔断器的窄接口。
//
// 流水线在遍历候选渠道时询问它是否放行该渠道，并在一次上游尝试结束后上报该渠道的结果；
// 具体实现由装配层注入（生产装配注入 internal/router 的断路器实现），
// 流水线不依赖具体实现，与 transport.RequestMetrics 同一手法：端口由消费方定义。
type Breaker interface {
	// Allow 报告该渠道当前是否放行一次调度；返回 false 时流水线跳过该候选。
	Allow(upstreamID string) bool
	// Record 上报一次该渠道调用的结果；err 为 nil 表示成功。
	//
	// 实现方自行判定哪些错误计入熔断：流水线只如实上报本次尝试的错误。
	Record(upstreamID string, err error)
}

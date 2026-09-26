package pipeline

import (
	"net/http"

	"github.com/sumwai/nova/internal/domain"
)

// CostEstimate 是一次尝试的成本上界，按额度层的 metric 分量给出。
//
// 流水线只填它知道的分量：requests 恒 1，tokens 取本次请求与渠道共同决定的有效输出
// 上限（取不到时至少 1）。usd 与 credits 由装配层的额度实现按价格层折算，流水线不引入
// 价格知识。零值分量表示该分量没有已知上界，额度层不得据此放宽准入。
type CostEstimate struct {
	Requests float64
	Tokens   float64
	USD      float64
	Credits  float64
}

// LimitRuntime 是额度层在转发路径上的接口，由装配层注入。
//
// 用接口而不是让流水线 import internal/limits：流水线只依赖 domain，额度实现带着档案与
// 持久化，不该下沉到核心转发路径。流水线只传事实——本次路由、请求模型、响应头与错误——
// 是否写入账号状态、保留多久、有没有连续失败计数，全部由额度层决定。
//
// 为 nil 时额度层整体不参与，转发行为与本层引入之前一致。
type LimitRuntime interface {
	// Reserve 原子地判定并预留一次上游尝试。第二个返回值为 false 时本次候选不可用，
	// 流水线跳过它、不计入尝试次数。est 是本次请求的成本上界，按 metric 分量给出。
	//
	// 返回的 settle 必须在上游尝试结束后调用一次：成功时按 actual 提交用量，失败时回滚预留。
	// settle 幂等，内部实现决定提交与回滚的判据（按 err 是否为 nil）。
	Reserve(route domain.Route, model string, est CostEstimate) (settle func(actual domain.Usage, err error), ok bool)
	// Observe 把一次上游尝试的响应头交给额度层做观测。succeeded 表示本次尝试成功，
	// 额度层据此清除学习到的不可用标记。
	Observe(route domain.Route, model string, header http.Header, succeeded bool)
	// LearnExhausted 把一次额度耗尽失败交给额度层，由它按自适应规则决定是否写入
	// 「至 until 不可用」的标记。err 可能携带上游的 Retry-After 提示。
	LearnExhausted(route domain.Route, model string, err error)
}

// quotaExhausted 报告错误是否为上游额度耗尽。
func quotaExhausted(err error) bool {
	domainErr := domain.AsError(err)
	return domainErr != nil && domainErr.Code == domain.CodeUpstreamQuotaExhausted
}

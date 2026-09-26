package limits

import (
	"math"
	"time"
)

// entry 是额度表里的一条条目：常量来自声明，状态来自观测与本地累计。
//
// baseline 是「某个已知时刻的剩余量」：观测到达时取观测值，仅有声明时取声明推算值。
// localUsed 是 baseline 之后本地记账的用量。二者相减即当前剩余量，因此「改善需一次
// 实测确认」只需决定是否替换 baseline，本地累计不会丢失。
type entry struct {
	key  Key
	spec windowSpec

	// 声明常量。
	limit         *float64
	declRemaining *float64
	declUsed      *float64
	unbounded     bool

	// 状态。
	baseline    *float64
	localUsed   float64
	windowStart time.Time
	resetsAt    time.Time
	observedAt  time.Time
	hasObserved bool
	// assumed 表示 baseline 来自声明而非实测；stale 表示观测已过期，两者都让
	// Available 报估算态，但只有 stale 会施加保守系数（声明是常量，不是记账缺口）。
	assumed bool
	stale   bool
	// pending 是等待一次实测确认的改善值；恶化立即生效，改善要连续两次才落地。
	pending *float64

	expiresAt time.Time

	// reserved 是已预留、尚未提交或回滚的估量。
	reserved float64
}

// declaredBaseline 由声明推算初始剩余量；无法确定时返回 nil。
func (e *entry) declaredBaseline() *float64 {
	if e.unbounded {
		return nil
	}
	switch {
	case e.declRemaining != nil:
		value := *e.declRemaining
		return &value
	case e.limit != nil && e.declUsed != nil:
		value := *e.limit - *e.declUsed
		return &value
	case e.limit != nil:
		value := *e.limit
		return &value
	default:
		return nil
	}
}

// freshBaseline 返回新窗口开始时的基准：声明上限。
//
// 窗口切换后剩余的额度回到上限，声明里的 remaining 是上一个窗口的状态，不再适用。
func (e *entry) freshBaseline() *float64 {
	if e.limit != nil {
		return floatPtr(*e.limit)
	}
	return nil
}

// applyObservation 并入一次新鲜观测。
//
// 恶化立即生效：观测值小于当前基准时直接替换，上游限流造成的选路抖动因此被立刻抑制。
// 改善需一次实测确认：第一次看到更高的值只记为待确认，第二次仍不低于该值才落地，
// 避免一个瞬时的高剩余量让账号立刻被重新选中。
func (e *entry) applyObservation(value float64) {
	switch {
	case e.baseline == nil:
		e.baseline = floatPtr(value)
		e.pending = nil
	case value < *e.baseline:
		e.baseline = floatPtr(value)
		e.pending = nil
	case value > *e.baseline:
		if e.pending != nil && value >= *e.pending {
			e.baseline = floatPtr(value)
			e.pending = nil
		} else {
			e.pending = floatPtr(value)
		}
	default:
		e.pending = nil
	}
	// 观测值已经包含此前的实际用量，本地累计从这里重新起算，避免重复扣减。
	e.localUsed = 0
}

// rollover 在窗口已经切换时清零本地状态。
//
// 滚动窗口没有跨窗口的历史：过期的条目丢弃本地累计，基准回到声明上限；自然周期同理。
// absolute 没有边界，永不切换。
func (e *entry) rollover(now time.Time) {
	if e.spec.kind == windowAbsolute {
		return
	}
	if e.windowStart.IsZero() {
		e.windowStart = e.spec.start(now)
		return
	}
	if now.Before(e.spec.end(e.windowStart)) {
		return
	}
	e.windowStart = e.spec.start(now)
	e.localUsed = 0
	e.pending = nil
	e.baseline = e.freshBaseline()
	e.assumed = e.baseline != nil
	e.stale = false
	e.hasObserved = false
	e.resetsAt = time.Time{}
}

// remaining 返回当前剩余量与两个标志。
//
// known=false 表示没有任何可用的剩余量数据，此时不阻断（无法证明耗尽），但调用方应把它
// 当作估算态。conservative 表示该值依赖本地累计而非权威观测，需要施加保守系数。
func (e *entry) remaining() (float64, bool, bool) {
	if e.unbounded {
		return math.Inf(1), true, e.stale
	}
	if e.baseline == nil {
		return 0, false, e.stale
	}
	return *e.baseline - e.localUsed, true, e.stale
}

// freshnessLimit 返回观测的新鲜度上限：min(TTLFactor × TTL, 窗口长度的 10%)。
//
// absolute 没有窗口长度，取更严格的 TTLFactor × TTL；窗口长度未知时同样退回该值。
func (e *entry) freshnessLimit(opts Options) time.Duration {
	base := time.Duration(opts.TTLFactor * float64(opts.TTL))
	if e.spec.kind == windowAbsolute {
		return base
	}
	length := e.spec.length
	if e.spec.kind == windowNatural && !e.windowStart.IsZero() {
		length = e.spec.lengthFrom(e.windowStart)
	}
	if length <= 0 {
		return base
	}
	if cap := length / 10; cap < base {
		return cap
	}
	return base
}

// floatPtr 返回值的指针副本。
func floatPtr(value float64) *float64 {
	return &value
}

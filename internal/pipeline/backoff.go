package pipeline

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"time"
)

// 换候选前退避的默认参数。
const (
	// defaultBackoffBase 是退避基准时长：第 n 次尝试失败后按 base × 2^(n-1) 增长。
	defaultBackoffBase = 200 * time.Millisecond
	// defaultBackoffMax 是单次退避上限：上游建议或指数退避算出的基准超过该值时不再等待，
	// 直接换下一候选（已是最后一个候选时立即失败），避免把客户端请求长时间挂在网关上。
	defaultBackoffMax = 3 * time.Second
	// maxBackoffShift 是计算 base × 2^(n-1) 时的最大左移位数。
	// time.Duration 是 int64，再大的左移必然溢出，故提前按「远超上限」处理。
	maxBackoffShift = 62
)

// BackoffOptions 描述可重试失败后、换下一候选前的退避策略。零值字段取对应默认值。
type BackoffOptions struct {
	// Base 是退避基准时长；<= 0 时取 defaultBackoffBase。
	Base time.Duration
	// Max 是单次退避上限；<= 0 时取 defaultBackoffMax。基准超过该上限时不等待。
	Max time.Duration
	// Jitter 返回 [0, n) 内的随机数，用于把等待时长打散；nil 时取 math/rand/v2 的全局源。
	// 测试注入它可得到确定的等待时长。
	Jitter func(n int64) int64
	// Wait 等待指定时长并支持上下文取消；nil 时用定时器实现。
	// 测试注入它可在不真实等待的前提下断言退避时长。
	Wait func(ctx context.Context, d time.Duration) error
}

// backoff 是退避策略的运行时形态，无状态，可并发使用。
type backoff struct {
	base   time.Duration
	max    time.Duration
	jitter func(n int64) int64
	wait   func(ctx context.Context, d time.Duration) error
}

// newBackoff 归一化退避参数：零值与非法值一律取默认值。
func newBackoff(opts BackoffOptions) backoff {
	b := backoff{
		base:   opts.Base,
		max:    opts.Max,
		jitter: opts.Jitter,
		wait:   opts.Wait,
	}
	if b.base <= 0 {
		b.base = defaultBackoffBase
	}
	if b.max <= 0 {
		b.max = defaultBackoffMax
	}
	if b.jitter == nil {
		b.jitter = defaultJitter
	}
	if b.wait == nil {
		b.wait = waitWithContext
	}
	return b
}

// defaultJitter 返回 [0, n) 内的随机数。退避抖动只用于打散重试时刻，
// 不参与任何安全判据，因此用非密码学随机源即可。
//
//nolint:gosec // G404：抖动不用于安全用途。
func defaultJitter(n int64) int64 {
	return rand.Int64N(n)
}

// retryAfterHint 是上游错误可选实现的能力：给出上游通过 Retry-After 响应头建议的退避时长。
//
// 接口就地声明为消费者契约：生产者是 internal/upstream 返回的限流错误，本包按结构匹配读取，
// 不导入该包，也不要求 domain 层感知 HTTP 响应头。
type retryAfterHint interface {
	RetryAfter() (time.Duration, bool)
}

// delay 返回第 failedAttempts 次尝试失败后、换下一候选前应等待的时长，并报告是否需要等待。
//
// 取值口径：
//
//   - 上游（例如 429 限流）给出 Retry-After 时以上游建议为基准，尊重上游要求；
//   - 否则按指数退避 base × 2^(failedAttempts-1) 增长；
//   - 基准超过上限时不等待，交由调用方立即换下一候选或快速失败；
//   - 上游建议立即重试（基准为零）时等待零时长，不改用指数退避；
//   - 否则在 [0, 基准) 内取随机抖动，避免并发请求在同一时刻同时重试。
//
// 第二个返回值为 false 时第一个返回值无意义，调用方不得等待。
func (b backoff) delay(failedAttempts int, err error) (time.Duration, bool) {
	base, hinted := b.retryAfter(err)
	if !hinted {
		base = b.exponential(failedAttempts)
	}
	// 上限一律生效：上游建议的等待与指数退避都不得超过上限。
	if base > b.max {
		return 0, false
	}
	if base <= 0 {
		// 只有上游明确建议立即重试时才会走到这里（指数退避的取值恒大于零）。
		return 0, true
	}
	return time.Duration(b.jitter(int64(base))), true
}

// retryAfter 取上游建议的退避时长，并报告错误是否携带该提示。
// 错误未实现该能力、声明不可用或给出负值时，一律按「没有提示」处理。
func (b backoff) retryAfter(err error) (time.Duration, bool) {
	var hint retryAfterHint
	if !errors.As(err, &hint) {
		return 0, false
	}
	delay, ok := hint.RetryAfter()
	if !ok || delay < 0 {
		return 0, false
	}
	return delay, true
}

// exponential 返回指数退避的基准时长 base × 2^(failedAttempts-1)。
//
// 溢出时返回 math.MaxInt64，使调用方按「远超上限、不等待」处理，
// 不把溢出的负值或零值误当成「无退避」以外的语义。
func (b backoff) exponential(failedAttempts int) time.Duration {
	if failedAttempts <= 1 {
		return b.base
	}
	shift := failedAttempts - 1
	if shift > maxBackoffShift {
		return math.MaxInt64
	}
	scaled := b.base << shift
	if scaled <= 0 {
		return math.MaxInt64
	}
	return scaled
}

// waitWithContext 用定时器等待 d，并在 ctx 取消时立即返回 ctx.Err()，
// 使退避等待不阻塞请求取消的传播。
func waitWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

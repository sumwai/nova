package limits

import (
	"sync"
	"time"

	"github.com/sumwai/nova/internal/profile"
)

// testClock 是可推进的假时钟，让窗口边界、TTL 与退避都能定死。
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

// newTestClock 返回一个从固定时刻起步的假时钟。
func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)}
}

// Now 返回当前假时刻。
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 推进假时钟。
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// ptr 返回值的指针副本，用于区分「缺失」与「零值」。
func ptr(value float64) *float64 {
	return &value
}

// accountQuota 构造一条账号级 quota 声明。
func accountQuota(window string, limit, used float64) profile.Limit {
	return profile.Limit{
		Kind:   "quota",
		Metric: "usd",
		Window: window,
		Limit:  ptr(limit),
		Used:   ptr(used),
	}
}

// declaredDoc 构造一份声明文档。
func declaredDoc(account []profile.Limit) *profile.LimitsDoc {
	return &profile.LimitsDoc{Schema: profile.CurrentSchema, Source: "profile", Account: account}
}

// observedDoc 构造一份带时刻的观测文档。
func observedDoc(at time.Time, account []profile.Limit) *profile.LimitsDoc {
	return &profile.LimitsDoc{
		Schema:     profile.CurrentSchema,
		Source:     "exec",
		ObservedAt: at.Format(time.RFC3339),
		Account:    account,
	}
}

// newTable 构造一张测试用额度表。
func newTable(clock *testClock, opts Options) *Table {
	opts.Account = "acct"
	opts.Clock = clock.Now
	if opts.FlushInterval == 0 {
		opts.FlushInterval = -1
	}
	return New(opts)
}

// accountsScope 返回默认账号的作用域。
func accountsScope() Scope {
	return Scope{Account: "acct"}
}

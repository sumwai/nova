package limits

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sumwai/nova/internal/profile"
)

func TestReserveReleaseRestoresCapacity(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	if err := table.MergeDeclared(declaredDoc([]profile.Limit{
		{Kind: "quota", Metric: "tokens", Window: "5h", Limit: ptr(1), Used: ptr(0)},
	})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}

	release, ok := table.Reserve(accountsScope(), "", Usage{Tokens: 1}, clock.Now())
	if !ok {
		t.Fatal("剩余 1、估量 1 应能预留")
	}
	if _, ok := table.Reserve(accountsScope(), "", Usage{Tokens: 1}, clock.Now()); ok {
		t.Fatal("预留已占满时不得再次预留")
	}
	release()
	if _, ok := table.Reserve(accountsScope(), "", Usage{Tokens: 1}, clock.Now()); !ok {
		t.Fatal("回滚后应恢复可预留容量")
	}
	release()
	release()
}

// TestReserveBlocksZeroRemaining 守护原子准入与 Available 一样挡住 remaining==0。
//
// 即使某个 metric 没有成本上界（估量分量为 0），remaining==0 也必须被挡住，
// 而不只是挡住负剩余。
func TestReserveBlocksZeroRemaining(t *testing.T) {
	clock := newTestClock()

	exhausted := newTable(clock, Options{})
	if err := exhausted.MergeDeclared(declaredDoc([]profile.Limit{
		{Kind: "quota", Metric: "usd", Window: "5h", Limit: ptr(10), Remaining: ptr(0)},
	})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	if _, ok := exhausted.Reserve(accountsScope(), "", Usage{}, clock.Now()); ok {
		t.Fatal("remaining==0 时即使该 metric 无估量也不得预留")
	}

	available := newTable(clock, Options{})
	if err := available.MergeDeclared(declaredDoc([]profile.Limit{
		{Kind: "quota", Metric: "usd", Window: "5h", Limit: ptr(10), Remaining: ptr(1)},
	})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	if _, ok := available.Reserve(accountsScope(), "", Usage{}, clock.Now()); !ok {
		t.Fatal("remaining>0 时无估量应放行")
	}
}

// TestReservePreventsOversellWithRequestsEstimate 守护生产口径：
// 流水线对 requests 恒传估量 1，剩余 1 时并发只能放行一个。
func TestReservePreventsOversellWithRequestsEstimate(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	if err := table.MergeDeclared(declaredDoc([]profile.Limit{
		{Kind: "quota", Metric: "requests", Window: "1m", Limit: ptr(60), Used: ptr(59)},
	})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}

	const attempts = 100
	var granted int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := table.Reserve(accountsScope(), "", Usage{Requests: 1}, clock.Now()); ok {
				atomic.AddInt32(&granted, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if granted != 1 {
		t.Fatalf("剩余 1 时并发授予 %d 次，期望恰好 1 次（不超卖）", granted)
	}
}

// TestReserveAppliesEstimatePerMetric 守护估量按 metric 分量扣减：
// requests 与 tokens 并存时，各自只扣自己的分量，互不折算。
func TestReserveAppliesEstimatePerMetric(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	if err := table.MergeDeclared(declaredDoc([]profile.Limit{
		{Kind: "quota", Metric: "requests", Window: "1m", Limit: ptr(2), Used: ptr(0)},
		{Kind: "quota", Metric: "tokens", Window: "1m", Limit: ptr(1000), Used: ptr(0)},
	})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}

	requestsKey := Key{Scope: Scope{Account: "acct"}, Metric: "requests", Window: "1m", Kind: "quota"}
	tokensKey := Key{Scope: Scope{Account: "acct"}, Metric: "tokens", Window: "1m", Kind: "quota"}

	release, ok := table.Reserve(accountsScope(), "", Usage{Requests: 1, Tokens: 400}, clock.Now())
	if !ok {
		t.Fatal("requests 2、tokens 1000 应能预留")
	}
	if got := table.entries[requestsKey].reserved; got != 1 {
		t.Fatalf("requests 预留=%v，期望 1", got)
	}
	if got := table.entries[tokensKey].reserved; got != 400 {
		t.Fatalf("tokens 预留=%v，期望 400", got)
	}

	if _, ok := table.Reserve(accountsScope(), "", Usage{Requests: 1, Tokens: 700}, clock.Now()); ok {
		t.Fatal("tokens 剩余 600 不足以覆盖 700 的估量，应整笔拒绝")
	}
	release()
	if _, ok := table.Reserve(accountsScope(), "", Usage{Requests: 1, Tokens: 700}, clock.Now()); !ok {
		t.Fatal("回滚后应恢复可预留容量")
	}
}

func TestReserveDoesNotOversellConcurrently(t *testing.T) {
	clock := newTestClock()
	table := newTable(clock, Options{})
	if err := table.MergeDeclared(declaredDoc([]profile.Limit{
		{Kind: "quota", Metric: "tokens", Window: "5h", Limit: ptr(100), Used: ptr(0)},
	})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}

	const attempts = 256
	var granted int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := table.Reserve(accountsScope(), "", Usage{Tokens: 1}, clock.Now()); ok {
				atomic.AddInt32(&granted, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if granted != 100 {
		t.Fatalf("并发预留授予 %d 次，期望恰好 100 次（不超卖）", granted)
	}
}

package stats

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/transport"
)

// testClock 是可推进的时钟：裁剪边界与时间窗因此可以定在确定位置。
type testClock struct {
	current time.Time
}

func newTestClock() *testClock {
	return &testClock{current: time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time { return c.current }

func (c *testClock) advance(d time.Duration) { c.current = c.current.Add(d) }

// testStore 造一个纯内存库的存储，测试结束自动关闭。
func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(Options{Now: newTestClock().now})
	if err != nil {
		t.Fatalf("New 意外失败：%v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func mustReport(t *testing.T, store *Store, q Query) Report {
	t.Helper()
	report, err := store.Report(q)
	if err != nil {
		t.Fatalf("Report 意外失败：%v", err)
	}
	return report
}

func accessRecord(requestID string, status int) transport.AccessRecord {
	return transport.AccessRecord{
		RequestID:  requestID,
		Protocol:   domain.ProtocolOpenAIChat,
		Model:      "gpt-5",
		HTTPStatus: status,
		DurationMS: 10,
		RemoteAddr: "127.0.0.1:33012",
		UserAgent:  "claude-cli/1.0.3",
	}
}

func attemptRecord(requestID, provider string, usage domain.Usage) domain.AttemptRecord {
	start := time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC)
	return domain.AttemptRecord{
		RequestID:        requestID,
		Attempt:          1,
		ClientProtocol:   domain.ProtocolOpenAIChat,
		UpstreamProtocol: domain.ProtocolOpenAIChat,
		RequestedModel:   "gpt-5",
		UpstreamID:       provider + " api.example.com",
		Provider:         provider,
		UpstreamModel:    "gpt-5",
		Outcome:          domain.AttemptOK,
		Usage:            usage,
		StartedAt:        start,
		EndedAt:          start.Add(5 * time.Millisecond),
	}
}

func upstreamUsage(input, output int) domain.Usage {
	return domain.Usage{
		Source:       domain.UsageSourceUpstream,
		InputTokens:  input,
		OutputTokens: output,
	}
}

func TestStoreJoinsAccessAndAttempts(t *testing.T) {
	store := testStore(t)
	if err := store.RecordAttempt(context.Background(), attemptRecord("r1", "relay-a", upstreamUsage(10, 20))); err != nil {
		t.Fatalf("RecordAttempt 意外失败：%v", err)
	}
	store.LogAccess(accessRecord("r1", 200))

	report := mustReport(t, store, Query{GroupBy: nil})
	if report.Window.Totals.Requests != 1 {
		t.Fatalf("窗口请求数 = %d，期望 1", report.Window.Totals.Requests)
	}
	if got := report.Window.Usage.InputTokens; got != 10 {
		t.Errorf("输入 token = %d，期望 10", got)
	}
	if got := report.Window.Usage.OutputTokens; got != 20 {
		t.Errorf("输出 token = %d，期望 20", got)
	}
	if len(report.Window.Requests) != 0 {
		t.Error("未要求明细时不应返回 requests")
	}
}

// 只有访问记录、没有尝试记录（选路失败、请求体解码失败）时仍要成一条记录：
// 这类请求算进了请求数，只是没有用量。
func TestStoreRecordsAccessWithoutAttempts(t *testing.T) {
	store := testStore(t)
	store.LogAccess(accessRecord("r1", 404))

	report := mustReport(t, store, Query{})
	if report.Window.Totals.Requests != 1 {
		t.Fatalf("窗口请求数 = %d，期望 1", report.Window.Totals.Requests)
	}
	if report.Window.Totals.Failed != 1 {
		t.Errorf("失败数 = %d，期望 1", report.Window.Totals.Failed)
	}
	if report.Lifetime.Requests != 1 {
		t.Errorf("累计请求数 = %d，期望 1", report.Lifetime.Requests)
	}
}

// 没有关联键的尝试无法归属，计入孤儿数而不是并入某条随机请求。
func TestStoreCountsOrphanAttempts(t *testing.T) {
	store := testStore(t)
	if err := store.RecordAttempt(context.Background(), attemptRecord("", "relay-a", upstreamUsage(1, 1))); err != nil {
		t.Fatalf("RecordAttempt 意外失败：%v", err)
	}
	report := mustReport(t, store, Query{})
	if report.Window.OrphanAttempts != 1 {
		t.Errorf("孤儿尝试数 = %d，期望 1", report.Window.OrphanAttempts)
	}
	if report.Window.Totals.Requests != 0 {
		t.Errorf("窗口请求数 = %d，期望 0", report.Window.Totals.Requests)
	}
}

// 从第一份已知用量起算，而不是从零值起算：从零值累加会让合并结果永远停在来源未知。
func TestStoreMergesKnownUsageAcrossAttempts(t *testing.T) {
	store := testStore(t)
	_ = store.RecordAttempt(context.Background(), attemptRecord("r1", "relay-a", domain.Usage{}))
	_ = store.RecordAttempt(context.Background(), attemptRecord("r1", "relay-b", upstreamUsage(7, 3)))
	store.LogAccess(accessRecord("r1", 200))

	report := mustReport(t, store, Query{})
	if got := report.Window.Usage.InputTokens; got != 7 {
		t.Errorf("输入 token = %d，期望 7", got)
	}
	if report.Window.Totals.UsageUnknownRequests != 1 {
		t.Errorf("用量未知请求数 = %d，期望 1", report.Window.Totals.UsageUnknownRequests)
	}
}

// 暂存超限时丢弃最旧的尝试；正常路径不会触发，这里验证它不丢记录整体。
func TestStoreEvictsOldestPending(t *testing.T) {
	store := testStore(t)
	for i := 0; i < pendingLimit+1; i++ {
		id := "req-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		_ = store.RecordAttempt(context.Background(), attemptRecord(id, "relay-a", upstreamUsage(1, 1)))
	}
	report := mustReport(t, store, Query{})
	if report.Window.DroppedPending != 1 {
		t.Errorf("暂存淘汰数 = %d，期望 1", report.Window.DroppedPending)
	}
}

// 跨重启持久化：重开同一个库，累计与明细都要还在，启动次数要递增。
func TestStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	clock := newTestClock()

	first, err := New(Options{Path: path, Now: clock.now})
	if err != nil {
		t.Fatalf("首次打开失败：%v", err)
	}
	_ = first.RecordAttempt(context.Background(), attemptRecord("r1", "relay-a", upstreamUsage(11, 22)))
	first.LogAccess(accessRecord("r1", 200))
	first.LogAccess(accessRecord("r2", 502))
	if err := first.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}

	// 重启：同一份文件、新的进程时刻。
	clock.advance(time.Minute)
	second, err := New(Options{Path: path, Now: clock.now})
	if err != nil {
		t.Fatalf("重开失败：%v", err)
	}
	defer func() { _ = second.Close() }()

	report := mustReport(t, second, Query{Detail: true, Limit: 10})
	if report.Lifetime.Requests != 2 {
		t.Errorf("重启后累计请求数 = %d，期望 2", report.Lifetime.Requests)
	}
	if got := report.Lifetime.Usage.InputTokens; got != 11 {
		t.Errorf("重启后累计输入 token = %d，期望 11", got)
	}
	if report.Process.Restarts != 1 {
		t.Errorf("启动次数 = %d，期望 1", report.Process.Restarts)
	}
	if !report.Accounting.Persisted {
		t.Error("落盘模式下 persisted 应为 true")
	}
	if report.Window.Totals.Requests != 2 {
		t.Errorf("重启后窗口请求数 = %d，期望 2", report.Window.Totals.Requests)
	}
	if len(report.Window.Requests) != 2 {
		t.Fatalf("重启后明细条数 = %d，期望 2", len(report.Window.Requests))
	}
	// 上游尝试随记录一起恢复：by_provider 依赖它。
	var providers []string
	for _, bucket := range mustReport(t, second, Query{GroupBy: []string{"provider"}}).Window.Breakdowns.ByProvider {
		providers = append(providers, bucket.Provider)
	}
	if len(providers) != 1 || providers[0] != "relay-a" {
		t.Errorf("重启后渠道分组 = %v，期望 [relay-a]", providers)
	}
}

// 裁剪只删明细，累计不变：这是 lifetime 与 requests 分表的唯一原因。
func TestStorePrunesRecordsButKeepsLifetime(t *testing.T) {
	clock := newTestClock()
	store, err := New(Options{Retention: time.Hour, Now: clock.now})
	if err != nil {
		t.Fatalf("New 意外失败：%v", err)
	}
	defer func() { _ = store.Close() }()

	store.LogAccess(accessRecord("r1", 200))

	// 越过保留期后再写一条：裁剪把 r1 从明细里删掉，累计仍为 2。
	clock.advance(2 * time.Hour)
	store.LogAccess(accessRecord("r2", 200))

	report := mustReport(t, store, Query{})
	if report.Lifetime.Requests != 2 {
		t.Errorf("累计请求数 = %d，期望 2（累计不受裁剪影响）", report.Lifetime.Requests)
	}
	if report.Window.Totals.Requests != 1 {
		t.Errorf("窗口请求数 = %d，期望 1（更早的已被裁剪）", report.Window.Totals.Requests)
	}
}

// 落库失败不改变累计镜像：库里的数字与报出来的数字必须一致。
func TestStoreWriteFailureKeepsLifetimeConsistent(t *testing.T) {
	store := testStore(t)
	store.LogAccess(accessRecord("r1", 200))
	before := store.lifetime.requests

	// 关掉底层库，后续写入必然失败。
	if err := store.db.Close(); err != nil {
		t.Fatalf("关闭底层库失败：%v", err)
	}
	store.LogAccess(accessRecord("r2", 200))

	if store.lifetime.requests != before {
		t.Errorf("写入失败后累计镜像 = %d，期望仍为 %d", store.lifetime.requests, before)
	}
	if store.writeErrors == 0 {
		t.Error("写入失败应被计数，使统计缺失在端点上可见")
	}
}

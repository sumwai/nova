package limits

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/profile"
)

func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	doc := declaredDoc([]profile.Limit{
		{Kind: "quota", Metric: "tokens", Window: "5h", Limit: ptr(100), Used: ptr(0)},
	})

	first := New(Options{Account: "acct", StateDir: dir, Clock: clock.Now, FlushInterval: -1})
	if err := first.MergeDeclared(doc); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	first.Consume(accountsScope(), "", Usage{Tokens: 30}, clock.Now())
	if err := first.Flush(); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}

	second := New(Options{Account: "acct", StateDir: dir, Clock: clock.Now, FlushInterval: -1})
	if degraded, reason := second.Degraded(); degraded {
		t.Fatalf("合法快照不应降级：%s", reason)
	}
	if err := second.MergeDeclared(doc); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	key := Key{Scope: Scope{Account: "acct"}, Metric: "tokens", Window: "5h", Kind: "quota"}
	item := second.entries[key]
	if item == nil {
		t.Fatal("快照状态未接到条目上")
	}
	if item.localUsed != 30 {
		t.Fatalf("本地累计未往返：localUsed=%v，期望 30", item.localUsed)
	}
	if got := *item.baseline - item.localUsed; got != 70 {
		t.Fatalf("剩余量未往返：%v，期望 70", got)
	}
}

// TestClosedTableDoesNotWriteBack 守护换出后的旧表不得把状态写回同一份快照。
//
// reload 先 Close 旧表、再让新表载入；在途请求可能在新表接管后才 settle，
// 旧表只应改内存状态，不得再落盘。
func TestClosedTableDoesNotWriteBack(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	doc := declaredDoc([]profile.Limit{
		{Kind: "quota", Metric: "tokens", Window: "5h", Limit: ptr(100), Used: ptr(0)},
	})

	first := New(Options{Account: "acct", StateDir: dir, Clock: clock.Now, FlushInterval: -1})
	if err := first.MergeDeclared(doc); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	first.Consume(accountsScope(), "", Usage{Tokens: 10}, clock.Now())
	if err := first.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("重复关闭应幂等且不报错：%v", err)
	}

	// Close 之后的变更不得再写回：这模拟在途请求在新表接管后才 settle。
	first.Consume(accountsScope(), "", Usage{Tokens: 90}, clock.Now())

	second := New(Options{Account: "acct", StateDir: dir, Clock: clock.Now, FlushInterval: -1})
	if err := second.MergeDeclared(doc); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	key := Key{Scope: Scope{Account: "acct"}, Metric: "tokens", Window: "5h", Kind: "quota"}
	if got := second.entries[key].localUsed; got != 10 {
		t.Fatalf("Close 后的变更写回了快照：localUsed=%v，期望 10", got)
	}
}

func TestCorruptSnapshotDegrades(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "limits.json"), []byte("{ 不是 JSON"), 0o600); err != nil {
		t.Fatalf("写入损坏快照失败：%v", err)
	}

	clock := newTestClock()
	table := New(Options{Account: "acct", StateDir: dir, Clock: clock.Now, FlushInterval: -1})
	if degraded, reason := table.Degraded(); !degraded || reason == "" {
		t.Fatal("损坏快照应把表标记为估算态并给出原因")
	}
	if err := table.MergeDeclared(declaredDoc([]profile.Limit{accountQuota("5h", 100, 0)})); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	ok, reason := table.Available(accountsScope(), "", clock.Now())
	if !ok {
		t.Fatalf("损坏快照不应阻断可用性判定，reason=%+v", reason)
	}
	if !reason.Estimated {
		t.Fatal("估算态必须在 Reason 里体现")
	}
}

func TestSnapshotGapDegrades(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	doc := declaredDoc([]profile.Limit{accountQuota("5h", 100, 0)})

	first := New(Options{Account: "acct", StateDir: dir, Clock: clock.Now, FlushInterval: -1})
	if err := first.MergeDeclared(doc); err != nil {
		t.Fatalf("合并声明失败：%v", err)
	}
	if err := first.Flush(); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}

	// 快照距今超过 GapThreshold：记账有断档，状态不可信。
	clock.Advance(2 * time.Hour)
	second := New(Options{Account: "acct", StateDir: dir, Clock: clock.Now, FlushInterval: -1, GapThreshold: time.Hour})
	if degraded, _ := second.Degraded(); !degraded {
		t.Fatal("时间上有断档应把表标记为估算态")
	}
}

func TestSnapshotWriteFailureIsObservable(t *testing.T) {
	dir := t.TempDir()
	// 用同名文件占住目录位置，使 MkdirAll 与 CreateTemp 都失败。
	if err := os.WriteFile(filepath.Join(dir, "blocked"), []byte("x"), 0o600); err != nil {
		t.Fatalf("准备失败：%v", err)
	}
	clock := newTestClock()
	table := New(Options{Account: "acct", StateDir: filepath.Join(dir, "blocked", "sub"), Clock: clock.Now, FlushInterval: -1})
	if err := table.Flush(); err == nil {
		t.Fatal("写盘失败应返回错误")
	}
	if degraded, _ := table.Degraded(); !degraded {
		t.Fatal("写盘失败应把表标记为估算态")
	}
}

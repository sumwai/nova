package limits

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// snapshotVersion 是快照文件格式代数。读取到不支持的版本即降级，不做猜测性迁移。
const snapshotVersion = 1

// snapshot 是 <StateDir>/limits.json 的内容。
//
// 只存状态，不存声明：上限、窗口与作用域来自额度文档，进程重启后由调用方重新合并。
// 声明与状态分开，快照损坏时丢的只是观测与本地累计，声明仍然完整，账号不会因此被
// 误判为可用。
type snapshot struct {
	Version   int             `json:"version"`
	WrittenAt time.Time       `json:"written_at"`
	Entries   []snapshotEntry `json:"entries"`
	Blocks    []snapshotBlock `json:"blocks,omitempty"`
}

// snapshotEntry 是一条条目的持久化形态。
type snapshotEntry struct {
	Account     string    `json:"account"`
	Pool        string    `json:"pool,omitempty"`
	Model       string    `json:"model,omitempty"`
	Metric      string    `json:"metric"`
	Window      string    `json:"window"`
	Kind        string    `json:"kind"`
	Baseline    *float64  `json:"baseline,omitempty"`
	LocalUsed   float64   `json:"local_used,omitempty"`
	WindowStart time.Time `json:"window_start"`
	ResetsAt    time.Time `json:"resets_at"`
	ObservedAt  time.Time `json:"observed_at"`
	HasObserved bool      `json:"has_observed,omitempty"`
	Assumed     bool      `json:"assumed,omitempty"`
	Stale       bool      `json:"stale,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// snapshotBlock 是一条学习到的不可用标记的持久化形态。
type snapshotBlock struct {
	Account       string    `json:"account"`
	Pool          string    `json:"pool,omitempty"`
	Model         string    `json:"model,omitempty"`
	Until         time.Time `json:"until"`
	Verdict       Verdict   `json:"verdict"`
	RequiresClear bool      `json:"requires_clear,omitempty"`
}

// snapshotPath 返回快照文件路径。
func (t *Table) snapshotPath() string {
	return filepath.Join(t.opts.StateDir, "limits.json")
}

// load 载入上一进程的快照。
//
// 载入失败不返回错误、也不让构造失败：状态不可信不等于账号不可用，因此把表标记为
// 估算态，由 Available 的 Reason 体现。偏保守，但不阻断。
func (t *Table) load() {
	if t.opts.StateDir == "" {
		return
	}
	data, err := os.ReadFile(t.snapshotPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		t.degrade(fmt.Sprintf("读取额度快照 %s 失败：%v", t.snapshotPath(), err))
		return
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.degrade(fmt.Sprintf("额度快照 %s 不是合法 JSON：%v", t.snapshotPath(), err))
		return
	}
	if snap.Version != snapshotVersion {
		t.degrade(fmt.Sprintf("额度快照版本 %d 不被本二进制支持（支持 %d）", snap.Version, snapshotVersion))
		return
	}

	now := t.now()
	switch {
	case snap.WrittenAt.IsZero():
		t.degrade("额度快照缺少写入时刻")
	case snap.WrittenAt.After(now):
		t.degrade("额度快照的写入时刻晚于本机时刻，时钟不可信")
	case now.Sub(snap.WrittenAt) > t.opts.GapThreshold:
		t.degrade(fmt.Sprintf("额度快照距今 %s，超过 %s 的记账连续性上限", now.Sub(snap.WrittenAt).Round(time.Second), t.opts.GapThreshold))
	}

	for _, state := range snap.Entries {
		if state.tooOld(now) {
			continue
		}
		t.pending[state.key()] = state
	}
	for _, saved := range snap.Blocks {
		t.blocks[blockKey{Account: saved.Account, Pool: saved.Pool, Model: saved.Model}] = block{
			until:         saved.Until,
			verdict:       saved.Verdict,
			requiresClear: saved.RequiresClear,
		}
	}
	t.lastFlush = now
}

// maybeFlush 在节流间隔之外落盘；间隔为负表示每次变更都落盘。
//
// Close 之后直接返回：旧装配换出后仍可能有在途请求调 Consume，它只能改内存状态，
// 不得再把旧状态写回已被新装配接管的同一份快照。
func (t *Table) maybeFlush(now time.Time) {
	if t.closed || t.opts.StateDir == "" || !t.dirty {
		return
	}
	if t.opts.FlushInterval > 0 && now.Sub(t.lastFlush) < t.opts.FlushInterval {
		return
	}
	t.flushLocked(now)
}

// Flush 立即落盘，忽略节流；进程退出前调用它。
//
// 它不置 closed：Flush 是「把当前状态推到磁盘」，之后表仍可能继续记账。
func (t *Table) Flush() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.flushLocked(t.now())
}

// Close 落盘并结束使用：落盘一次后置 closed，之后的变更不再落盘。
//
// 幂等：重复调用不重复落盘，也不报错——装配换出与进程退出可能各调一次。
func (t *Table) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	err := t.flushLocked(t.now())
	t.closed = true
	return err
}

// flushLocked 以临时文件 + rename 写快照。
//
// 写盘失败即把表标记为估算态：下一次载入看到的可能是上一份旧状态，静默继续会让
// 剩余量偏乐观。临时文件先 fsync 再 rename，掉电后不会留下半份文件。
func (t *Table) flushLocked(now time.Time) error {
	if t.opts.StateDir == "" {
		t.dirty = false
		return nil
	}
	snap := snapshot{
		Version:   snapshotVersion,
		WrittenAt: now,
		Entries:   make([]snapshotEntry, 0, len(t.entries)),
	}
	for _, item := range t.entries {
		snap.Entries = append(snap.Entries, item.snapshot())
	}
	for key, item := range t.blocks {
		snap.Blocks = append(snap.Blocks, snapshotBlock{
			Account:       key.Account,
			Pool:          key.Pool,
			Model:         key.Model,
			Until:         item.until,
			Verdict:       item.verdict,
			RequiresClear: item.requiresClear,
		})
	}

	data, err := json.MarshalIndent(&snap, "", "  ")
	if err != nil {
		t.degrade(fmt.Sprintf("序列化额度快照失败：%v", err))
		return err
	}
	if err := os.MkdirAll(t.opts.StateDir, 0o700); err != nil {
		t.degrade(fmt.Sprintf("创建状态目录 %s 失败：%v", t.opts.StateDir, err))
		return err
	}

	file := t.snapshotPath()
	tmp, err := os.CreateTemp(t.opts.StateDir, "limits.json.tmp-*")
	if err != nil {
		t.degrade(fmt.Sprintf("创建临时快照文件失败：%v", err))
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		t.degrade(fmt.Sprintf("写入临时快照文件失败：%v", err))
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		t.degrade(fmt.Sprintf("同步临时快照文件失败：%v", err))
		return err
	}
	if err := tmp.Close(); err != nil {
		t.degrade(fmt.Sprintf("关闭临时快照文件失败：%v", err))
		return err
	}
	if err := os.Rename(tmpName, file); err != nil {
		t.degrade(fmt.Sprintf("替换额度快照失败：%v", err))
		return err
	}
	// rename 只保证目录项可见，目录项本身要 fsync 目录才会落盘。
	if dir, err := os.Open(t.opts.StateDir); err == nil {
		dir.Sync()
		dir.Close()
	}

	t.dirty = false
	t.lastFlush = now
	return nil
}

// degrade 把表标记为估算态，只保留第一条原因。
func (t *Table) degrade(reason string) {
	if t.degraded {
		return
	}
	t.degraded = true
	t.degradedReason = reason
}

// Degraded 报告本表是否处于估算态，以及原因。
func (t *Table) Degraded() (bool, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.degraded, t.degradedReason
}

// DefaultBlockCap 返回 quota 条目在 resets_at 缺席时的不可用上限。
//
// 调用方（额度层之外的学习逻辑）用它与上游 Retry-After 择一决定 until。
// opts 在 New 之后不再改动，因此无需加锁。
func (t *Table) DefaultBlockCap() time.Duration {
	return t.opts.BlockCap
}

// snapshot 返回条目的持久化形态。
func (e *entry) snapshot() snapshotEntry {
	return snapshotEntry{
		Account:     e.key.Account,
		Pool:        e.key.Pool,
		Model:       e.key.Model,
		Metric:      e.key.Metric,
		Window:      e.key.Window,
		Kind:        e.key.Kind,
		Baseline:    e.baseline,
		LocalUsed:   e.localUsed,
		WindowStart: e.windowStart,
		ResetsAt:    e.resetsAt,
		ObservedAt:  e.observedAt,
		HasObserved: e.hasObserved,
		Assumed:     e.assumed,
		Stale:       e.stale,
		ExpiresAt:   e.expiresAt,
	}
}

// key 还原快照条目的键。
func (s snapshotEntry) key() Key {
	return Key{
		Scope:  Scope{Account: s.Account, Pool: s.Pool, Model: s.Model},
		Metric: s.Metric,
		Window: s.Window,
		Kind:   s.Kind,
	}
}

// apply 把快照状态接到条目上。声明已经补齐窗口与上限，这里只恢复状态。
func (s snapshotEntry) apply(item *entry) {
	item.baseline = s.Baseline
	item.localUsed = s.LocalUsed
	item.windowStart = s.WindowStart
	item.resetsAt = s.ResetsAt
	item.observedAt = s.ObservedAt
	item.hasObserved = s.HasObserved
	item.assumed = s.Assumed
	item.stale = s.Stale
	item.expiresAt = s.ExpiresAt
}

// tooOld 报告快照状态是否已经超过一个完整窗口。
//
// 超过窗口长度的条目在 rollover 里本来就会被清零，载入时直接丢弃可以少一次替换；
// 保留它们只会让一个早已作废的剩余量短暂参与求值。absolute 没有窗口，永不丢弃。
func (s snapshotEntry) tooOld(now time.Time) bool {
	switch s.Window {
	case "absolute", "day", "week", "month", "":
		return false
	}
	length, err := parseDurationWindow(s.Window)
	if err != nil || s.WindowStart.IsZero() {
		return false
	}
	return now.Sub(s.WindowStart) > length
}

// parseObservedAt 解析观测时刻；缺失或非法都表示没有可用的时间锚点。
func parseObservedAt(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

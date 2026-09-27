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

// maybeFlush 在节流间隔之外安排一次后台落盘；间隔为负表示每次变更都安排。
//
// 真正写盘在独立 goroutine 里做：序列化与 fsync 不再占着表的互斥锁，
// 触发本次落盘的那个请求因此不必等磁盘。同一时刻只允许一个在途写盘（flushing），
// 期间的变更照旧置 dirty，由下一次触发或 Close 收尾。
//
// Close 之后直接返回：旧装配换出后仍可能有在途请求调 Consume，它只能改内存状态，
// 不得再把旧状态写回已被新装配接管的同一份快照。
func (t *Table) maybeFlush(now time.Time) {
	if t.closed || t.opts.StateDir == "" || !t.dirty || t.flushing {
		return
	}
	if t.opts.FlushInterval > 0 && now.Sub(t.lastFlush) < t.opts.FlushInterval {
		return
	}
	t.flushing = true
	t.writers.Add(1)
	go func() {
		defer t.writers.Done()
		_ = t.writeLatest()
		t.mu.Lock()
		t.flushing = false
		t.mu.Unlock()
	}()
}

// Flush 立即落盘，忽略节流；进程退出前调用它。
//
// 它不置 closed：Flush 是「把当前状态推到磁盘」，之后表仍可能继续记账。
func (t *Table) Flush() error {
	return t.writeLatest()
}

// Close 落盘并结束使用：落盘一次后置 closed，之后的变更不再落盘。
//
// 幂等：重复调用不重复落盘，也不报错——装配换出与进程退出可能各调一次。
// 置 closed 后先等在途写盘结束，再写最终快照，因此不会出现旧快照覆盖新快照。
func (t *Table) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	t.writers.Wait()
	return t.writeLatest()
}

// writeLatest 取当前状态并写盘，全程串行化。
//
// 快照在持有 writeMu 时重建，因此最后一次写入一定是最新状态：多个写者不会
// 让一份旧快照盖掉新快照。快照取出后立即清 dirty，写盘期间新到的变更会重新置位，
// 写成功后不再碰它——这样“写盘期间发生的变更”不会被错误地当成已落盘。
// writeMu 先于 t.mu 获取，全局只有这一个锁序，不存在与其他路径的反向嵌套。
func (t *Table) writeLatest() error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	t.mu.Lock()
	if t.opts.StateDir == "" {
		t.dirty = false
		t.mu.Unlock()
		return nil
	}
	now := t.now()
	snap := t.snapshotLocked(now)
	t.dirty = false
	t.mu.Unlock()

	err := writeSnapshot(t.opts.StateDir, t.snapshotPath(), &snap)

	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		// 写盘失败即把表标记为估算态：下一次载入看到的可能是上一份旧状态，
		// 静默继续会让剩余量偏乐观。变更仍未落盘，dirty 置回真，留给下一次触发。
		t.degrade(err.Error())
		t.dirty = true
	}
	t.lastFlush = now
	return err
}

// snapshotLocked 把当前条目与标记复制成一份可离盘序列化的快照；调用方需持有 t.mu。
func (t *Table) snapshotLocked(now time.Time) snapshot {
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
	return snap
}

// writeSnapshot 以临时文件 + rename 写一份快照。
//
// 临时文件先 fsync 再 rename，掉电后不会留下半份文件；rename 只保证目录项可见，
// 目录项本身再 fsync 一次。错误已经带上了失败的步骤，调用方直接拿去标估算态。
func writeSnapshot(stateDir, file string, snap *snapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化额度快照失败：%v", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("创建状态目录 %s 失败：%v", stateDir, err)
	}

	tmp, err := os.CreateTemp(stateDir, "limits.json.tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时快照文件失败：%v", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时快照文件失败：%v", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("同步临时快照文件失败：%v", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时快照文件失败：%v", err)
	}
	if err := os.Rename(tmpName, file); err != nil {
		return fmt.Errorf("替换额度快照失败：%v", err)
	}
	if dir, err := os.Open(stateDir); err == nil {
		dir.Sync()
		dir.Close()
	}
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

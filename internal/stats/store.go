package stats

import (
	"context"
	"database/sql"
	"strconv"
	"sync"
	"time"

	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/transport"
)

// 缺省取值。
const (
	// DefaultRetention 是请求记录的保留期。
	//
	// 请求记录按保留期裁剪，lifetime 永不裁剪：前者随时间无上限增长，后者是常数。
	DefaultRetention = 30 * 24 * time.Hour
	// DefaultScanLimit 是单次查询最多读入内存的记录条数。
	//
	// 上限存在的意义是给「无下界的查询」一个上界：没有它时一次 `since` 缺省的查询
	// 会把整个保留期读进内存。触到上限时查询结果会标出来，而不是静默返回一部分。
	DefaultScanLimit = 200000
	// pendingLimit 是 join 暂存的在飞请求数上限。
	pendingLimit = 1024
	// pruneInterval 是两次裁剪之间的最小间隔。
	pruneInterval = time.Hour
)

// Options 是构造 Store 的入参。
type Options struct {
	// Path 是 SQLite 文件路径；空表示纯内存库（测试与不落盘的调用方）。
	Path string
	// Retention 是请求记录的保留期；<= 0 时取 DefaultRetention。
	Retention time.Duration
	// ScanLimit 是单次查询的读入上限；<= 0 时取 DefaultScanLimit。
	ScanLimit int
	// Now 是取当前时刻的函数，测试用它把裁剪边界与时间窗定在确定位置。
	Now func() time.Time
}

// Store 是统计存储：SQLite 保存事实，内存只放 join 暂存与累计缓存。
//
// 它同时实现入口层的 transport.AccessLogger 与流水线的 domain.Observer：两条事实流
// 在这里按 request_id 合并成一条请求记录后落库。合并在写入侧完成，落库的每一条
// 都是自洽的完整记录，读侧因此只需一次按时间范围的查询。
//
// 生命周期与装配解耦：reload 会整体换掉 Assembly 与其中的日志句柄，统计库由进程持有。
type Store struct {
	db        *sql.DB
	closeOnce sync.Once

	mu        sync.Mutex
	now       func() time.Time
	retention time.Duration
	scanLimit int
	startedAt time.Time
	since     time.Time
	restarts  int64
	lastPrune time.Time
	// persisted 报告本次是否用了磁盘上的库；为假时累计会随重启归零。
	persisted bool

	// pending 暂存同一 request_id 的上游尝试，等访问记录到达时合并。
	pending        map[string]*pendingRequest
	droppedPending int64
	orphanAttempts int64

	// lifetime 是库中累计行的内存镜像。写入成功后才更新，因此它与库里的数字
	// 始终一致；读侧用它避免每次查询都再读一遍单行表。
	lifetime lifetimeCounters

	// writeErrors / lastWriteError 记录落库失败。
	//
	// 这些错误不外抛（观测失败不得影响转发），但也不能悄悄消失：计数与最后一条
	// 消息随查询结果一起返回，使「统计为什么少了」在端点自身上可见，
	// 而不是只能翻日志。
	writeErrors    int64
	lastWriteError string
}

// pendingRequest 是一个请求尚未与访问记录合并的上游尝试。
type pendingRequest struct {
	attempts []AttemptSummary
	first    time.Time
}

// lifetimeCounters 是跨重启的标量累计。刻意只有标量，见 Lifetime 的说明。
type lifetimeCounters struct {
	requests             int64
	succeeded            int64
	failed               int64
	retriedRequests      int64
	usageUnknownRequests int64
	usage                usageTotals
}

// New 打开统计库、恢复累计并做一次启动裁剪。
func New(opts Options) (*Store, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	retention := opts.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}
	scanLimit := opts.ScanLimit
	if scanLimit <= 0 {
		scanLimit = DefaultScanLimit
	}

	db, err := openDB(opts.Path)
	if err != nil {
		return nil, err
	}

	store := &Store{
		db:        db,
		now:       now,
		retention: retention,
		scanLimit: scanLimit,
		startedAt: now(),
		persisted: opts.Path != "",
		pending:   make(map[string]*pendingRequest),
	}
	if err := store.loadMeta(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.loadLifetime(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := pruneRequests(db, store.startedAt.Add(-retention)); err != nil {
		_ = db.Close()
		return nil, err
	}
	store.lastPrune = store.startedAt
	return store, nil
}

// Close 关闭统计库。
//
// 由进程在退出前调用：不关会让 WAL 与 shm 文件留在磁盘上，下一次启动要靠恢复流程
// 处理它们，而正常退出本可以没有这一步。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var err error
	s.closeOnce.Do(func() { err = s.db.Close() })
	return err
}

// loadMeta 读出（首次则写入）累计起点与启动次数。
func (s *Store) loadMeta() error {
	raw, ok, err := readMeta(s.db, metaSince)
	if err != nil {
		return err
	}
	if ok {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return err
		}
		s.since = parsed
		restarts, _, err := readMetaInt(s.db, metaRestarts)
		if err != nil {
			return err
		}
		s.restarts = int64(restarts) + 1
	} else {
		s.since = s.startedAt
		s.restarts = 0
	}
	if err := writeMeta(s.db, metaSince, s.since.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return writeMeta(s.db, metaRestarts, strconv.FormatInt(s.restarts, 10))
}

func (s *Store) loadLifetime() error {
	counters, err := readLifetime(s.db)
	if err != nil {
		return err
	}
	s.lifetime = counters
	return nil
}

// LogAccess 写一条请求记录，实现 transport.AccessLogger。
//
// 恒不返回错误：统计是尽力而为的观测，落库失败也不能影响转发结果。失败会计数并
// 记下最后一条消息，随查询结果返回。
func (s *Store) LogAccess(rec transport.AccessRecord) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	req := s.buildRequestLocked(rec)
	s.pruneLocked()
	if err := s.persistLocked(req); err != nil {
		s.noteWriteErrorLocked(err)
	}
}

// RecordAttempt 暂存一条上游尝试，实现 domain.Observer。
//
// 恒返回 nil：观测失败不得影响转发结果，而暂存除了内存之外没有别的失败模式。
func (s *Store) RecordAttempt(_ context.Context, rec domain.AttemptRecord) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 没有关联键的尝试无法与任何访问记录配对：计入孤儿数而不是并入某条随机请求，
	// 那会让归属错误且无从察觉。
	if rec.RequestID == "" {
		s.orphanAttempts++
		return nil
	}

	pending := s.pending[rec.RequestID]
	if pending == nil {
		pending = &pendingRequest{first: s.now()}
		s.pending[rec.RequestID] = pending
	}
	pending.attempts = append(pending.attempts, AttemptSummary{
		Provider:   rec.Provider,
		Upstream:   rec.UpstreamID,
		Account:    rec.AccountRef,
		Outcome:    string(rec.Outcome),
		ErrorCode:  rec.ErrorCode,
		DurationMS: rec.EndedAt.Sub(rec.StartedAt).Milliseconds(),
		Usage:      rec.Usage,
	})
	s.evictPendingLocked()
	return nil
}

// buildRequestLocked 把一条访问记录与其暂存的尝试合并成一条请求记录。
func (s *Store) buildRequestLocked(rec transport.AccessRecord) Request {
	req := Request{
		Time:         s.now(),
		RequestID:    rec.RequestID,
		Protocol:     string(rec.Protocol),
		Model:        rec.Model,
		Stream:       rec.Stream,
		Status:       rec.HTTPStatus,
		DurationMS:   rec.DurationMS,
		WrittenBytes: rec.WrittenBytes,
		ErrorCode:    rec.ErrorCode,
		RemoteAddr:   rec.RemoteAddr,
		UserAgent:    rec.UserAgent,
		Client:       NormalizeClient(rec.UserAgent),
	}
	if rec.RequestID != "" {
		if pending := s.pending[rec.RequestID]; pending != nil {
			req.Attempts = pending.attempts
			delete(s.pending, rec.RequestID)
		}
	}

	// 从第一份已知用量起算、而不是从零值起算：Usage.Add 会把「来源未知」判为
	// 较不可信的一方，从零值累加会让合并结果永远停在来源未知。
	haveUsage := false
	for i := range req.Attempts {
		attempt := &req.Attempts[i]
		if !attempt.Usage.Known() {
			req.UsageUnknown++
			continue
		}
		if !haveUsage {
			req.Usage = attempt.Usage
			haveUsage = true
			continue
		}
		req.Usage = req.Usage.Add(attempt.Usage)
	}
	req.Providers = distinctProviders(req.Attempts)
	return req
}

// persistLocked 在一个事务里落库，并在提交成功后更新内存里的累计镜像。
func (s *Store) persistLocked(req Request) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := insertRequest(tx, req); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	s.lifetime.requests++
	if req.Status < 400 {
		s.lifetime.succeeded++
	} else {
		s.lifetime.failed++
	}
	if len(req.Attempts) > 1 {
		s.lifetime.retriedRequests++
	}
	if req.UsageUnknown > 0 {
		s.lifetime.usageUnknownRequests++
	}
	s.lifetime.usage.add(req.Usage)
	return nil
}

// noteWriteErrorLocked 记下落库失败；第一条例外是唯一一次值得报的消息。
func (s *Store) noteWriteErrorLocked(err error) {
	s.writeErrors++
	if s.lastWriteError == "" {
		s.lastWriteError = err.Error()
	}
}

// pruneLocked 按保留期裁剪请求记录，最多每小时做一次。
//
// 裁剪不碰 lifetime：累计是「一直以来的合计」，不该因为保留期而变小。
func (s *Store) pruneLocked() {
	moment := s.now()
	if !s.lastPrune.IsZero() && moment.Sub(s.lastPrune) < pruneInterval {
		return
	}
	s.lastPrune = moment
	if _, err := pruneRequests(s.db, moment.Add(-s.retention)); err != nil {
		s.noteWriteErrorLocked(err)
	}
}

// evictPendingLocked 在 join 暂存超限时丢弃最旧的一条。
//
// 丢弃最旧而不是最新，是为了让仍在进行中的请求有机会保持完整。
func (s *Store) evictPendingLocked() {
	if len(s.pending) <= pendingLimit {
		return
	}
	var oldestKey string
	var oldest *pendingRequest
	for key, pending := range s.pending {
		if oldest == nil || pending.first.Before(oldest.first) {
			oldestKey, oldest = key, pending
		}
	}
	delete(s.pending, oldestKey)
	s.droppedPending++
}

// Report 按查询条件聚合。
//
// 记录从库里按时间范围读出，其余过滤与聚合在 Go 侧完成：过滤语义是 pattern.Match，
// SQL 的 LIKE / GLOB 与它不等价，下推会让同一条件在两条路径上给出不同结果。
func (s *Store) Report(q Query) (Report, error) {
	records, limited, err := s.load(q)
	if err != nil {
		return Report{}, err
	}

	s.mu.Lock()
	meta := reportMeta{
		now:             s.now(),
		startedAt:       s.startedAt,
		accountingSince: s.since,
		restarts:        s.restarts,
		lifetime:        s.lifetime,
		retention:       s.retention,
		scanLimited:     limited,
		droppedPending:  s.droppedPending,
		orphanAttempts:  s.orphanAttempts,
		writeErrors:     s.writeErrors,
		lastWriteError:  s.lastWriteError,
		persisted:       s.persisted,
	}
	s.mu.Unlock()

	return buildReport(records, meta, q), nil
}

// load 读出查询范围内的记录。
//
// since 缺省时退到保留期起点：比它更早的记录已经不在库里，把下界收到这里
// 让「无下界查询」等价于「整个保留期」，而不是一个没有边界的全表扫描。
func (s *Store) load(q Query) ([]Request, bool, error) {
	floor := s.now().Add(-s.retention)
	since := q.Since
	if since.IsZero() || since.Before(floor) {
		since = floor
	}
	return loadRequests(s.db, since, q.Until, s.scanLimit)
}

// distinctProviders 返回去重后的渠道名，按首现顺序。
func distinctProviders(attempts []AttemptSummary) []string {
	if len(attempts) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(attempts))
	var providers []string
	for _, attempt := range attempts {
		if attempt.Provider == "" || seen[attempt.Provider] {
			continue
		}
		seen[attempt.Provider] = true
		providers = append(providers, attempt.Provider)
	}
	return providers
}

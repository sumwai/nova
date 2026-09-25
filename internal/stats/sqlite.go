package stats

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 实现：不引入 CGO，保住单文件构建

	"github.com/sumwai/nova/internal/domain"
)

// schemaVersion 是状态库的表结构版本。
//
// 它是磁盘格式的契约，与程序版本、配置语法代数都不同：一次不兼容的表结构变更
// 才会推动它 +1。读到一个更高的版本时按「本二进制不认识这份库」处理，
// 不猜字段也不尝试降级——猜错的后果是把历史数字读成别的意思。
const schemaVersion = 1

// sqliteDriver 是 modernc.org/sqlite 注册的驱动名。
const sqliteDriver = "sqlite"

// schemaSQL 建表。
//
// requests 与 lifetime 分开：requests 按保留期裁剪，lifetime 永不删。
// 「累计用量不能因为裁剪而变小」是这两张表分开的唯一原因。
//
// attempts 以 JSON 文本存在 requests 的一列里，而不是另开一张表：过滤与聚合
// 都在 Go 侧按 pattern.Match 的语义做，SQL 不需要索引尝试行；拆表只会在
// 读路径上多一次 join，并多出一份需要跟内存结构保持同步的映射。
const schemaSQL = `
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS lifetime (
    id                     INTEGER PRIMARY KEY CHECK (id = 1),
    requests               INTEGER NOT NULL,
    succeeded              INTEGER NOT NULL,
    failed                 INTEGER NOT NULL,
    retried_requests       INTEGER NOT NULL,
    usage_unknown_requests INTEGER NOT NULL,
    input_tokens           INTEGER NOT NULL,
    output_tokens          INTEGER NOT NULL,
    cache_read_tokens      INTEGER NOT NULL,
    cache_write_tokens     INTEGER NOT NULL,
    reasoning_tokens       INTEGER NOT NULL,
    server_tool_uses       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS requests (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    time_ns           INTEGER NOT NULL,
    request_id        TEXT    NOT NULL,
    protocol          TEXT    NOT NULL,
    model             TEXT    NOT NULL,
    stream            INTEGER NOT NULL,
    status            INTEGER NOT NULL,
    duration_ms       INTEGER NOT NULL,
    written_bytes     INTEGER NOT NULL,
    error_code        TEXT    NOT NULL,
    remote_addr       TEXT    NOT NULL,
    user_agent        TEXT    NOT NULL,
    client            TEXT    NOT NULL,
    providers         TEXT    NOT NULL,
    usage_unknown     INTEGER NOT NULL,
    usage_source      TEXT    NOT NULL,
    input_tokens      INTEGER NOT NULL,
    output_tokens     INTEGER NOT NULL,
    cache_read_tokens INTEGER NOT NULL,
    cache_write_tokens INTEGER NOT NULL,
    reasoning_tokens  INTEGER NOT NULL,
    server_tool_uses  INTEGER NOT NULL,
    attempts          TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS requests_time ON requests(time_ns);
`

// openDB 打开（必要时创建）状态库并确认表结构可用。
func openDB(path string) (*sql.DB, error) {
	dsn := memoryDSN
	if path != "" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("创建状态目录 %s 失败：%w", dir, err)
			}
		}
		dsn = "file:" + path
	}
	// WAL 让读不阻塞写；busy_timeout 把偶发锁等待变成等待而不是立即报错；
	// synchronous=NORMAL 在 WAL 下已能保证进程崩溃不丢已提交事务，且不必每次 fsync。
	dsn += "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"

	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("打开统计库失败：%w", err)
	}
	// 单连接：本进程是唯一写者，而 SQLite 的并发写只会带来 SQLITE_BUSY 这类噪声。
	// 统计是本机工具，串行化的代价可以接受，换来的是「不会因为并发写而丢记录」。
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接统计库失败：%w", err)
	}
	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// memoryDSN 是纯内存库的 DSN，供测试与「不落盘」的调用方使用。
const memoryDSN = "file::memory:"

func initSchema(db *sql.DB) error {
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("建表失败：%w", err)
	}

	stored, ok, err := readMetaInt(db, metaSchemaVersion)
	if err != nil {
		return err
	}
	switch {
	case !ok:
		if err := writeMeta(db, metaSchemaVersion, strconv.Itoa(schemaVersion)); err != nil {
			return err
		}
	case stored > schemaVersion:
		return fmt.Errorf("统计库的表结构版本是 %d，本二进制只认到 %d；请升级 nova", stored, schemaVersion)
	}

	// lifetime 恒有一行，写路径只做 UPDATE，读路径只做 SELECT，不必处理「行不存在」。
	if _, err := db.Exec(`INSERT OR IGNORE INTO lifetime
        (id, requests, succeeded, failed, retried_requests, usage_unknown_requests,
         input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, server_tool_uses)
        VALUES (1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)`); err != nil {
		return fmt.Errorf("初始化累计行失败：%w", err)
	}
	return nil
}

// meta 键名。
const (
	metaSchemaVersion = "schema_version"
	metaSince         = "accounting_since"
	metaRestarts      = "restarts"
)

func readMetaInt(db *sql.DB, key string) (int, bool, error) {
	raw, ok, err := readMeta(db, key)
	if err != nil || !ok {
		return 0, ok, err
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false, fmt.Errorf("统计库里的 %s 取值 %q 不是整数", key, raw)
	}
	return value, true, nil
}

func readMeta(db *sql.DB, key string) (string, bool, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("读取统计库元信息失败：%w", err)
	default:
		return value, true, nil
	}
}

func writeMeta(db *sql.DB, key, value string) error {
	_, err := db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
        ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("写入统计库元信息失败：%w", err)
	}
	return nil
}

// insertRequest 在一个事务里写入一条请求记录并累加累计行。
//
// 两者同一事务：分开写会出现「请求行在、累计没涨」或反过来的状态，而两种状态都会
// 让同一个事实出现两个互相矛盾的数字。
func insertRequest(tx *sql.Tx, req Request) error {
	attempts, err := json.Marshal(encodeAttempts(req.Attempts))
	if err != nil {
		return fmt.Errorf("编码上游尝试失败：%w", err)
	}
	providers, err := json.Marshal(req.Providers)
	if err != nil {
		return fmt.Errorf("编码渠道列表失败：%w", err)
	}

	if _, err := tx.Exec(`INSERT INTO requests (
            time_ns, request_id, protocol, model, stream, status, duration_ms, written_bytes,
            error_code, remote_addr, user_agent, client, providers, usage_unknown, usage_source,
            input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
            reasoning_tokens, server_tool_uses, attempts)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		req.Time.UnixNano(), req.RequestID, req.Protocol, req.Model, boolToInt(req.Stream),
		req.Status, req.DurationMS, req.WrittenBytes, req.ErrorCode, req.RemoteAddr,
		req.UserAgent, req.Client, string(providers), req.UsageUnknown, string(req.Usage.Source),
		req.Usage.InputTokens, req.Usage.OutputTokens, req.Usage.CacheReadTokens,
		req.Usage.CacheWriteTokens, req.Usage.ReasoningTokens, req.Usage.ServerToolUses,
		string(attempts)); err != nil {
		return fmt.Errorf("写入请求记录失败：%w", err)
	}

	_, err = tx.Exec(`UPDATE lifetime SET
            requests = requests + 1,
            succeeded = succeeded + ?,
            failed = failed + ?,
            retried_requests = retried_requests + ?,
            usage_unknown_requests = usage_unknown_requests + ?,
            input_tokens = input_tokens + ?,
            output_tokens = output_tokens + ?,
            cache_read_tokens = cache_read_tokens + ?,
            cache_write_tokens = cache_write_tokens + ?,
            reasoning_tokens = reasoning_tokens + ?,
            server_tool_uses = server_tool_uses + ?
        WHERE id = 1`,
		boolToInt(req.Status < 400), boolToInt(req.Status >= 400),
		boolToInt(len(req.Attempts) > 1), boolToInt(req.UsageUnknown > 0),
		req.Usage.InputTokens, req.Usage.OutputTokens, req.Usage.CacheReadTokens,
		req.Usage.CacheWriteTokens, req.Usage.ReasoningTokens, req.Usage.ServerToolUses)
	if err != nil {
		return fmt.Errorf("累加用失败：%w", err)
	}
	return nil
}

// loadRequests 读出查询范围内的请求记录，按时刻升序。
//
// 只按时间下推给 SQL，其余过滤一律留给 Go：过滤语义是 pattern.Match（`*` 跨 `/`、
// 大小写按 rune 折叠），SQL 的 LIKE 与 GLOB 都不等价，下推会让同一份查询条件
// 在两条路径上给出不同结果。
//
// 多取一条用于判断是否触到扫描上限：返回的 limited 为真表示还有更早的记录没被读到。
func loadRequests(db *sql.DB, since, until time.Time, limit int) ([]Request, bool, error) {
	rows, err := db.Query(`SELECT
            time_ns, request_id, protocol, model, stream, status, duration_ms, written_bytes,
            error_code, remote_addr, user_agent, client, providers, usage_unknown, usage_source,
            input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
            reasoning_tokens, server_tool_uses, attempts
        FROM requests WHERE time_ns >= ? AND time_ns <= ? ORDER BY time_ns LIMIT ?`,
		lowerBound(since), upperBound(until), limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("读取统计记录失败：%w", err)
	}
	defer func() { _ = rows.Close() }()

	records := make([]Request, 0, 64)
	for rows.Next() {
		record, err := scanRequest(rows)
		if err != nil {
			return nil, false, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("读取统计记录失败：%w", err)
	}
	if len(records) > limit {
		return records[:limit], true, nil
	}
	return records, false, nil
}

// scanner 是 *sql.Row 与 *sql.Rows 共有的最小接口，两者都实现 Scan。
type scanner interface {
	Scan(dest ...any) error
}

func scanRequest(row scanner) (Request, error) {
	var (
		record    Request
		timeNS    int64
		stream    int
		providers string
		source    string
		attempts  string
	)
	err := row.Scan(&timeNS, &record.RequestID, &record.Protocol, &record.Model, &stream,
		&record.Status, &record.DurationMS, &record.WrittenBytes, &record.ErrorCode,
		&record.RemoteAddr, &record.UserAgent, &record.Client, &providers, &record.UsageUnknown,
		&source, &record.Usage.InputTokens, &record.Usage.OutputTokens,
		&record.Usage.CacheReadTokens, &record.Usage.CacheWriteTokens,
		&record.Usage.ReasoningTokens, &record.Usage.ServerToolUses, &attempts)
	if err != nil {
		return Request{}, fmt.Errorf("解析统计记录失败：%w", err)
	}

	record.Time = time.Unix(0, timeNS)
	record.Stream = stream != 0
	record.Usage.Source = domain.UsageSource(source)
	if err := json.Unmarshal([]byte(providers), &record.Providers); err != nil {
		return Request{}, fmt.Errorf("解析渠道列表失败：%w", err)
	}
	var encodedAttempts []attemptDTO
	if err := json.Unmarshal([]byte(attempts), &encodedAttempts); err != nil {
		return Request{}, fmt.Errorf("解析上游尝试失败：%w", err)
	}
	record.Attempts = decodeAttempts(encodedAttempts)
	return record, nil
}

// readLifetime 读出累计行。
func readLifetime(db *sql.DB) (lifetimeCounters, error) {
	var (
		counters lifetimeCounters
		usage    usageTotals
	)
	err := db.QueryRow(`SELECT requests, succeeded, failed, retried_requests,
            usage_unknown_requests, input_tokens, output_tokens, cache_read_tokens,
            cache_write_tokens, reasoning_tokens, server_tool_uses
        FROM lifetime WHERE id = 1`).Scan(
		&counters.requests, &counters.succeeded, &counters.failed, &counters.retriedRequests,
		&counters.usageUnknownRequests, &usage.input, &usage.output, &usage.cacheRead,
		&usage.cacheWrite, &usage.reasoning, &usage.tools)
	if err != nil {
		return lifetimeCounters{}, fmt.Errorf("读取累计用量失败：%w", err)
	}
	counters.usage = usage
	return counters, nil
}

// pruneRequests 删除早于 cutoff 的请求记录，返回删除条数。
//
// 只删 requests：lifetime 是「一直以来的合计」，不随保留期变小。
func pruneRequests(db *sql.DB, cutoff time.Time) (int64, error) {
	result, err := db.Exec(`DELETE FROM requests WHERE time_ns < ?`, cutoff.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("裁剪统计记录失败：%w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取裁剪条数失败：%w", err)
	}
	return deleted, nil
}

// lowerBound 把零值时刻解释为「无下界」。
//
// 零值 time.Time 的 UnixNano 有确定取值但那是个巧合，写死 math.MinInt64 才能让
// 「没有 since」这件事在 SQL 里有确切含义。
func lowerBound(t time.Time) int64 {
	if t.IsZero() {
		return math.MinInt64
	}
	return t.UnixNano()
}

// upperBound 把零值时刻解释为「无上界」。
//
// 与 lowerBound 对称：缺省查询不带 until，若直接取零值的 UnixNano（一个大负数），
// 条件会变成「时间戳小于某个负数」，结果是一条都匹配不上，而不是全部匹配。
func upperBound(t time.Time) int64 {
	if t.IsZero() {
		return math.MaxInt64
	}
	return t.UnixNano()
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// usageDTO 是用量在磁盘上的形状。
//
// 与内存结构分开定义：磁盘格式是长期契约，内存结构可以随代码重构，直接复用会让
// 一次字段重命名悄悄改变历史文件的含义。
type usageDTO struct {
	Source           string `json:"source,omitempty"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int    `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int    `json:"reasoning_tokens,omitempty"`
	ServerToolUses   int    `json:"server_tool_uses,omitempty"`
}

// attemptDTO 是一条上游尝试在磁盘上的形状。
type attemptDTO struct {
	Provider   string   `json:"provider,omitempty"`
	Upstream   string   `json:"upstream,omitempty"`
	Account    string   `json:"account,omitempty"`
	Outcome    string   `json:"outcome"`
	ErrorCode  string   `json:"error_code,omitempty"`
	DurationMS int64    `json:"duration_ms"`
	Usage      usageDTO `json:"usage"`
}

func encodeAttempts(attempts []AttemptSummary) []attemptDTO {
	if len(attempts) == 0 {
		return nil
	}
	encoded := make([]attemptDTO, 0, len(attempts))
	for _, attempt := range attempts {
		encoded = append(encoded, attemptDTO{
			Provider:   attempt.Provider,
			Upstream:   attempt.Upstream,
			Account:    attempt.Account,
			Outcome:    attempt.Outcome,
			ErrorCode:  attempt.ErrorCode,
			DurationMS: attempt.DurationMS,
			Usage: usageDTO{
				Source:           string(attempt.Usage.Source),
				InputTokens:      attempt.Usage.InputTokens,
				OutputTokens:     attempt.Usage.OutputTokens,
				CacheReadTokens:  attempt.Usage.CacheReadTokens,
				CacheWriteTokens: attempt.Usage.CacheWriteTokens,
				ReasoningTokens:  attempt.Usage.ReasoningTokens,
				ServerToolUses:   attempt.Usage.ServerToolUses,
			},
		})
	}
	return encoded
}

func decodeAttempts(encoded []attemptDTO) []AttemptSummary {
	if len(encoded) == 0 {
		return nil
	}
	decoded := make([]AttemptSummary, 0, len(encoded))
	for _, item := range encoded {
		decoded = append(decoded, AttemptSummary{
			Provider:   item.Provider,
			Upstream:   item.Upstream,
			Account:    item.Account,
			Outcome:    item.Outcome,
			ErrorCode:  item.ErrorCode,
			DurationMS: item.DurationMS,
			Usage: domain.Usage{
				Source:           domain.UsageSource(item.Usage.Source),
				InputTokens:      item.Usage.InputTokens,
				OutputTokens:     item.Usage.OutputTokens,
				CacheReadTokens:  item.Usage.CacheReadTokens,
				CacheWriteTokens: item.Usage.CacheWriteTokens,
				ReasoningTokens:  item.Usage.ReasoningTokens,
				ServerToolUses:   item.Usage.ServerToolUses,
			},
		})
	}
	return decoded
}

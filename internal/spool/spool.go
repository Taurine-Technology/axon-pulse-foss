// Package spool provides a crash-safe SQLite upload queue with a bounded
// logical telemetry payload budget.
package spool

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	_ "modernc.org/sqlite"
)

type (
	Kind string

	Record struct {
		Kind      Kind
		Timestamp int64
		Payload   []byte
	}

	Options struct {
		MaxPayloadBytes int64
		MaxAge          time.Duration
		Now             func() time.Time
	}

	Stats struct {
		Records           int64 `json:"records"`
		Bytes             int64 `json:"bytes"`
		PayloadBytes      int64 `json:"payload_bytes"`
		PayloadLimitBytes int64 `json:"payload_limit_bytes"`
		StorageBytes      int64 `json:"storage_bytes"`
		Dropped           int64 `json:"dropped"`
	}

	HistoryRecord struct {
		Kind      Kind            `json:"kind"`
		Timestamp int64           `json:"timestamp"`
		Data      json.RawMessage `json:"data"`
	}

	PendingBatch struct {
		Batch     protocol.Batch
		Body      []byte
		RecordIDs []int64
	}

	Spool struct {
		db              *sql.DB
		path            string
		maxPayloadBytes int64
		maxAge          time.Duration
		now             func() time.Time
	}

	queryRower interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}
)

const (
	DefaultMaxPayloadBytes = int64(50 << 20)
	DefaultMaxAge          = 7 * 24 * time.Hour
	maxBatchRecords        = 200

	unreservedRecordPredicate = `NOT EXISTS (
	SELECT 1 FROM pending_batch, json_each(pending_batch.record_ids)
	WHERE CAST(json_each.value AS INTEGER) = records.id
)`

	KindMinuteMetric Kind = "minute_metric"
	KindDNSCheck     Kind = "dns_check"
	KindHTTPCheck    Kind = "http_check"
	KindLinkContext  Kind = "link_context"
	KindSpeedTest    Kind = "speed_test"
	KindEvent        Kind = "event"
)

func NewRecord(kind Kind, timestamp int64, value any) (Record, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return Record{}, fmt.Errorf("encode %s record: %w", kind, err)
	}
	return Record{Kind: kind, Timestamp: timestamp, Payload: payload}, nil
}

func Open(path string, options Options) (*Spool, error) {
	if options.MaxPayloadBytes <= 0 {
		options.MaxPayloadBytes = DefaultMaxPayloadBytes
	}
	if options.MaxAge <= 0 {
		options.MaxAge = DefaultMaxAge
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create spool directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open spool: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	spool := &Spool{db: db, path: path, maxPayloadBytes: options.MaxPayloadBytes, maxAge: options.MaxAge, now: options.Now}
	if err := spool.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := spool.prune(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return spool, nil
}

func (s *Spool) initialize() error {
	statements := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA secure_delete=ON",
		"PRAGMA journal_size_limit=4194304",
		`CREATE TABLE IF NOT EXISTS records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL,
			ts INTEGER NOT NULL,
			payload BLOB NOT NULL,
			local_only INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		"CREATE INDEX IF NOT EXISTS records_ts_id ON records(ts, id)",
		`CREATE TABLE IF NOT EXISTS pending_batch (
			batch_id TEXT PRIMARY KEY,
			seq INTEGER NOT NULL UNIQUE,
			body BLOB NOT NULL,
			record_ids BLOB NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER NOT NULL UNIQUE,
			kind TEXT NOT NULL,
			ts INTEGER NOT NULL,
			payload BLOB NOT NULL,
			local_only INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		"CREATE INDEX IF NOT EXISTS history_ts_id ON history(ts, id)",
		`CREATE TABLE IF NOT EXISTS metadata (
			key TEXT PRIMARY KEY,
			value INTEGER NOT NULL
		)`,
		"INSERT OR IGNORE INTO metadata(key, value) VALUES ('next_seq', 1)",
		"INSERT OR IGNORE INTO metadata(key, value) VALUES ('dropped', 0)",
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize spool: %w", err)
		}
	}
	if err := s.ensureColumn("records", "local_only", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("history", "local_only", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if _, err := s.db.Exec("CREATE INDEX IF NOT EXISTS records_uploadable_id ON records(id) WHERE local_only = 0"); err != nil {
		return fmt.Errorf("index uploadable spool records: %w", err)
	}
	var integrity string
	if err := s.db.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil {
		return fmt.Errorf("check spool integrity: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("spool integrity check failed: %s", integrity)
	}
	return nil
}

func (s *Spool) ensureColumn(table, column, definition string) error {
	// Callers pass compile-time literals only. SQLite cannot bind identifiers
	// in PRAGMA or DDL, so externally influenced values must never reach here.
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan %s schema: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate %s schema: %w", table, err)
	}
	if _, err := s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition); err != nil {
		return fmt.Errorf("migrate %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Spool) Close() error {
	return s.db.Close()
}

// AppendMinute writes a complete aggregation interval in one transaction and
// prunes retention/cap overflow before committing.
func (s *Spool) AppendMinute(ctx context.Context, records []Record) (int64, error) {
	return s.append(ctx, records, false)
}

// AppendLocal retains local-mode telemetry for history and diagnostics while
// making it ineligible for a future controller upload.
func (s *Spool) AppendLocal(ctx context.Context, records []Record) (int64, error) {
	return s.append(ctx, records, true)
}

func (s *Spool) append(ctx context.Context, records []Record, localOnly bool) (int64, error) {
	if len(records) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin spool append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.PrepareContext(ctx, "INSERT INTO records(kind, ts, payload, local_only, created_at) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		return 0, fmt.Errorf("prepare spool append: %w", err)
	}
	now := s.now().Unix()
	for _, record := range records {
		if !validKind(record.Kind) || record.Timestamp <= 0 || len(record.Payload) == 0 {
			_ = statement.Close()
			return 0, fmt.Errorf("invalid spool record kind=%q timestamp=%d", record.Kind, record.Timestamp)
		}
		if _, err := statement.ExecContext(ctx, string(record.Kind), record.Timestamp, record.Payload, localOnly, now); err != nil {
			_ = statement.Close()
			return 0, fmt.Errorf("append spool record: %w", err)
		}
	}
	if err := statement.Close(); err != nil {
		return 0, fmt.Errorf("close spool statement: %w", err)
	}
	dropped, err := s.pruneTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit spool append: %w", err)
	}
	return dropped, nil
}

func (s *Spool) pruneTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	cutoff := s.now().Add(-s.maxAge).Unix()
	// Retry-stable batches share the spool retention bound. Expiring the batch
	// reservation first lets its records be pruned below, including raw SSIDs.
	if _, err := tx.ExecContext(ctx, "DELETE FROM pending_batch WHERE created_at < ?", cutoff); err != nil {
		return 0, fmt.Errorf("prune expired pending batches: %w", err)
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM records WHERE ts < ? AND "+unreservedRecordPredicate, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune expired spool rows: %w", err)
	}
	dropped, _ := result.RowsAffected()
	if _, err := tx.ExecContext(ctx, "DELETE FROM history WHERE ts < ?", cutoff); err != nil {
		return 0, fmt.Errorf("prune expired local history: %w", err)
	}

	bytesUsed, err := payloadBytes(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("measure spool: %w", err)
	}
	for bytesUsed > s.maxPayloadBytes {
		result, err := tx.ExecContext(ctx, "DELETE FROM history WHERE id IN (SELECT id FROM history ORDER BY id LIMIT 128)")
		if err != nil {
			return 0, fmt.Errorf("prune local history overflow: %w", err)
		}
		removed, _ := result.RowsAffected()
		if removed == 0 {
			break
		}
		bytesUsed, err = payloadBytes(ctx, tx)
		if err != nil {
			return 0, err
		}
	}
	for bytesUsed > s.maxPayloadBytes {
		rows, err := tx.QueryContext(ctx, "SELECT id, length(payload) FROM records WHERE "+unreservedRecordPredicate+" ORDER BY id LIMIT 128")
		if err != nil {
			return 0, fmt.Errorf("select spool overflow: %w", err)
		}
		ids := make([]int64, 0, 128)
		var removedBytes int64
		for rows.Next() {
			var id, size int64
			if err := rows.Scan(&id, &size); err != nil {
				_ = rows.Close()
				return 0, fmt.Errorf("scan spool overflow: %w", err)
			}
			ids = append(ids, id)
			removedBytes += size
			if bytesUsed-removedBytes <= s.maxPayloadBytes {
				break
			}
		}
		if err := rows.Close(); err != nil {
			return 0, fmt.Errorf("close overflow rows: %w", err)
		}
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("iterate spool overflow: %w", err)
		}
		if len(ids) == 0 {
			break
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM records WHERE id IN ("+placeholders(len(ids))+")", int64Args(ids)...); err != nil {
			return 0, fmt.Errorf("drop spool overflow: %w", err)
		}
		dropped += int64(len(ids))
		bytesUsed -= removedBytes
	}
	if dropped > 0 {
		if _, err := tx.ExecContext(ctx, "UPDATE metadata SET value = value + ? WHERE key = 'dropped'", dropped); err != nil {
			return 0, fmt.Errorf("count dropped spool rows: %w", err)
		}
	}
	return dropped, nil
}

func (s *Spool) prune(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin spool prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.pruneTx(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit spool prune: %w", err)
	}
	return nil
}

// NextBatch returns the existing retry-stable batch or atomically reserves the
// oldest records under a fresh UUIDv7 and sequence number.
func (s *Spool) NextBatch(ctx context.Context, appVersion string, configVersion int) (*PendingBatch, error) {
	if pending, err := s.loadPending(ctx, s.db); err != nil || pending != nil {
		return pending, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin batch reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if pending, err := s.loadPending(ctx, tx); err != nil || pending != nil {
		return pending, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, kind, payload FROM records WHERE local_only = 0 ORDER BY id LIMIT 400")
	if err != nil {
		return nil, fmt.Errorf("select spool records: %w", err)
	}
	batch := protocol.Batch{
		AppVersion: appVersion, ConfigVersion: configVersion,
		MinuteMetrics: []protocol.MinuteMetric{}, DNSChecks: []protocol.DNSCheck{},
		HTTPChecks: []protocol.HTTPCheck{}, LinkContext: []protocol.LinkContext{},
		SpeedTests: []json.RawMessage{}, Events: []protocol.Event{},
	}
	ids := make([]int64, 0, maxBatchRecords)
	for rows.Next() && len(ids) < maxBatchRecords {
		var id int64
		var kind string
		var payload []byte
		if err := rows.Scan(&id, &kind, &payload); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan spool record: %w", err)
		}
		included, err := addRecord(&batch, Kind(kind), payload)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("decode spool record %d: %w", id, err)
		}
		if included {
			ids = append(ids, id)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close spool records: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spool records: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if err := tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key = 'next_seq'").Scan(&batch.Sequence); err != nil {
		return nil, fmt.Errorf("read batch sequence: %w", err)
	}
	batch.SensorClock = s.now().Unix()
	batch.BatchID, err = protocol.NewBatchID(s.now())
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return nil, fmt.Errorf("encode batch: %w", err)
	}
	encodedIDs, _ := json.Marshal(ids)
	if _, err := tx.ExecContext(ctx, "INSERT INTO pending_batch(batch_id, seq, body, record_ids, created_at) VALUES (?, ?, ?, ?, ?)", batch.BatchID, batch.Sequence, body, encodedIDs, s.now().Unix()); err != nil {
		return nil, fmt.Errorf("persist pending batch: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE metadata SET value = value + 1 WHERE key = 'next_seq'"); err != nil {
		return nil, fmt.Errorf("advance batch sequence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit batch reservation: %w", err)
	}
	return &PendingBatch{Batch: batch, Body: body, RecordIDs: ids}, nil
}

func (s *Spool) loadPending(ctx context.Context, querier queryRower) (*PendingBatch, error) {
	var body, encodedIDs []byte
	err := querier.QueryRowContext(ctx, "SELECT body, record_ids FROM pending_batch ORDER BY seq LIMIT 1").Scan(&body, &encodedIDs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load pending batch: %w", err)
	}
	var batch protocol.Batch
	var ids []int64
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, fmt.Errorf("decode pending batch: %w", err)
	}
	if err := json.Unmarshal(encodedIDs, &ids); err != nil {
		return nil, fmt.Errorf("decode pending record IDs: %w", err)
	}
	return &PendingBatch{Batch: batch, Body: append([]byte(nil), body...), RecordIDs: ids}, nil
}

func (s *Spool) Ack(ctx context.Context, batchID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin batch ack: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var encodedIDs []byte
	if err := tx.QueryRowContext(ctx, "SELECT record_ids FROM pending_batch WHERE batch_id = ?", batchID).Scan(&encodedIDs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("load batch ack: %w", err)
	}
	var ids []int64
	if err := json.Unmarshal(encodedIDs, &ids); err != nil {
		return fmt.Errorf("decode batch ack IDs: %w", err)
	}
	if len(ids) > 0 {
		arguments := int64Args(ids)
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO history(source_id, kind, ts, payload, local_only, created_at) SELECT id, kind, ts, payload, local_only, created_at FROM records WHERE id IN ("+placeholders(len(ids))+")", arguments...); err != nil {
			return fmt.Errorf("retain acknowledged local history: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM records WHERE id IN ("+placeholders(len(ids))+")", arguments...); err != nil {
			return fmt.Errorf("delete acknowledged records: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM pending_batch WHERE batch_id = ?", batchID); err != nil {
		return fmt.Errorf("delete acknowledged batch: %w", err)
	}
	if _, err := s.pruneTx(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit batch ack: %w", err)
	}
	return nil
}

func (s *Spool) Stats(ctx context.Context) (Stats, error) {
	stats := Stats{PayloadLimitBytes: s.maxPayloadBytes}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(length(payload)), 0) FROM records WHERE local_only = 0").Scan(&stats.Records, &stats.Bytes); err != nil {
		return Stats{}, fmt.Errorf("read spool stats: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT (SELECT COALESCE(SUM(length(payload)), 0) FROM records) + (SELECT COALESCE(SUM(length(payload)), 0) FROM history)").Scan(&stats.PayloadBytes); err != nil {
		return Stats{}, fmt.Errorf("read spool payload usage: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key = 'dropped'").Scan(&stats.Dropped); err != nil {
		return Stats{}, fmt.Errorf("read dropped count: %w", err)
	}
	storageBytes, err := s.storageBytes()
	if err != nil {
		return Stats{}, err
	}
	stats.StorageBytes = storageBytes
	return stats, nil
}

func payloadBytes(ctx context.Context, querier queryRower) (int64, error) {
	var bytes int64
	err := querier.QueryRowContext(ctx, "SELECT (SELECT COALESCE(SUM(length(payload)), 0) FROM records) + (SELECT COALESCE(SUM(length(payload)), 0) FROM history)").Scan(&bytes)
	return bytes, err
}

func (s *Spool) storageBytes() (int64, error) {
	var total int64
	for _, path := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("measure spool storage %s: %w", filepath.Base(path), err)
		}
		total += info.Size()
	}
	return total, nil
}

func (s *Spool) History(ctx context.Context, since time.Time, limit int) ([]HistoryRecord, error) {
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, ts, payload FROM (
			SELECT kind, ts, payload, source_id FROM (
				SELECT kind, ts, payload, id AS source_id FROM records WHERE ts >= ?
				UNION ALL
				SELECT kind, ts, payload, source_id FROM history WHERE ts >= ?
			) ORDER BY ts DESC, source_id DESC LIMIT ?
		) ORDER BY ts ASC, source_id ASC`, since.Unix(), since.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("read local history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]HistoryRecord, 0)
	for rows.Next() {
		var record HistoryRecord
		if err := rows.Scan(&record.Kind, &record.Timestamp, &record.Data); err != nil {
			return nil, fmt.Errorf("scan local history: %w", err)
		}
		record.Data = append(json.RawMessage(nil), record.Data...)
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate local history: %w", err)
	}
	return result, nil
}

func (s *Spool) Clear(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin spool clear: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		"DELETE FROM pending_batch", "DELETE FROM records", "DELETE FROM history",
		"UPDATE metadata SET value = 1 WHERE key = 'next_seq'",
		"UPDATE metadata SET value = 0 WHERE key = 'dropped'",
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("clear spool: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit spool clear: %w", err)
	}
	var busy, logFrames, checkpointedFrames int
	if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		return fmt.Errorf("checkpoint cleared spool: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint cleared spool: database remained busy (%d log frames, %d checkpointed)", logFrames, checkpointedFrames)
	}
	return nil
}

func addRecord(batch *protocol.Batch, kind Kind, payload []byte) (bool, error) {
	switch kind {
	case KindMinuteMetric:
		if len(batch.MinuteMetrics) >= 60 {
			return false, nil
		}
		var value protocol.MinuteMetric
		if err := json.Unmarshal(payload, &value); err != nil {
			return false, err
		}
		batch.MinuteMetrics = append(batch.MinuteMetrics, value)
	case KindDNSCheck:
		if len(batch.DNSChecks) >= 60 {
			return false, nil
		}
		var value protocol.DNSCheck
		if err := json.Unmarshal(payload, &value); err != nil {
			return false, err
		}
		batch.DNSChecks = append(batch.DNSChecks, value)
	case KindHTTPCheck:
		if len(batch.HTTPChecks) >= 60 {
			return false, nil
		}
		var value protocol.HTTPCheck
		if err := json.Unmarshal(payload, &value); err != nil {
			return false, err
		}
		batch.HTTPChecks = append(batch.HTTPChecks, value)
	case KindLinkContext:
		if len(batch.LinkContext) >= 60 {
			return false, nil
		}
		var value protocol.LinkContext
		if err := json.Unmarshal(payload, &value); err != nil {
			return false, err
		}
		batch.LinkContext = append(batch.LinkContext, value)
	case KindSpeedTest:
		if len(batch.SpeedTests) >= 8 {
			return false, nil
		}
		if !json.Valid(payload) {
			return false, errors.New("invalid speed-test JSON")
		}
		batch.SpeedTests = append(batch.SpeedTests, append(json.RawMessage(nil), payload...))
	case KindEvent:
		if len(batch.Events) >= 100 {
			return false, nil
		}
		var value protocol.Event
		if err := json.Unmarshal(payload, &value); err != nil {
			return false, err
		}
		batch.Events = append(batch.Events, value)
	default:
		return false, fmt.Errorf("unknown record kind %q", kind)
	}
	return true, nil
}

func validKind(kind Kind) bool {
	switch kind {
	case KindMinuteMetric, KindDNSCheck, KindHTTPCheck, KindLinkContext, KindSpeedTest, KindEvent:
		return true
	default:
		return false
	}
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func int64Args(values []int64) []any {
	out := make([]any, len(values))
	for index, value := range values {
		out[index] = value
	}
	return out
}

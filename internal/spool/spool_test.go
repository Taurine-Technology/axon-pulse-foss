package spool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
)

func TestBatchIsStableUntilAcknowledged(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	path := t.TempDir() + "/spool.db"
	queue, err := Open(path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	metric := protocol.MinuteMetric{Timestamp: now.Unix(), Target: "anchor", RTT: protocol.RTTSummary{P50: 10, P95: 20, Min: 5, Max: 25}, JitterMS: 2, Samples: 60}
	record, err := NewRecord(KindMinuteMetric, now.Unix(), metric)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.AppendMinute(context.Background(), []Record{record}); err != nil {
		t.Fatal(err)
	}
	first, err := queue.NextBatch(context.Background(), "0.1.0", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, Options{Now: func() time.Time { return now.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	second, err := reopened.NextBatch(context.Background(), "different", 9)
	if err != nil {
		t.Fatal(err)
	}
	if first.Batch.BatchID != second.Batch.BatchID || first.Batch.Sequence != second.Batch.Sequence || string(first.Body) != string(second.Body) {
		t.Fatalf("retry batch changed: first=%+v second=%+v", first.Batch, second.Batch)
	}
	if err := reopened.Ack(context.Background(), first.Batch.BatchID); err != nil {
		t.Fatal(err)
	}
	stats, err := reopened.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 0 {
		t.Fatalf("records after ack = %d", stats.Records)
	}
	history, err := reopened.History(context.Background(), now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Kind != KindMinuteMetric {
		t.Fatalf("history = %+v", history)
	}
}

func TestLocalOnlyRecordsNeverEnterControllerBatch(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	queue, err := Open(t.TempDir()+"/spool.db", Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	local, _ := NewRecord(KindEvent, now.Unix(), protocol.Event{Timestamp: now.Unix(), Type: "local"})
	connected, _ := NewRecord(KindEvent, now.Add(time.Second).Unix(), protocol.Event{Timestamp: now.Add(time.Second).Unix(), Type: "connected"})
	if _, err := queue.AppendLocal(context.Background(), []Record{local}); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.AppendMinute(context.Background(), []Record{connected}); err != nil {
		t.Fatal(err)
	}
	if stats, err := queue.Stats(context.Background()); err != nil || stats.Records != 1 {
		t.Fatalf("upload-eligible stats = %+v, %v", stats, err)
	}
	batch, err := queue.NextBatch(context.Background(), "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	if batch == nil || len(batch.Batch.Events) != 1 || batch.Batch.Events[0].Type != "connected" {
		t.Fatalf("controller batch = %+v", batch)
	}
	if err := queue.Ack(context.Background(), batch.Batch.BatchID); err != nil {
		t.Fatal(err)
	}
	if next, err := queue.NextBatch(context.Background(), "test", 1); err != nil || next != nil {
		t.Fatalf("local row became upload eligible: batch=%+v err=%v", next, err)
	}
	history, err := queue.History(context.Background(), now.Add(-time.Minute), 10)
	if err != nil || len(history) != 2 {
		t.Fatalf("local history = %+v, %v", history, err)
	}
}

func TestSpoolDropsOldestAtCapAndCountsLoss(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	queue, err := Open(t.TempDir()+"/spool.db", Options{MaxPayloadBytes: 100, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	records := make([]Record, 0, 3)
	for i := range 3 {
		event := protocol.Event{Timestamp: now.Unix() + int64(i), Type: "offline_start", Detail: map[string]any{}}
		record, err := NewRecord(KindEvent, event.Timestamp, event)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	dropped, err := queue.AppendMinute(context.Background(), records)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := queue.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 || stats.Dropped != dropped || stats.Bytes > 100 {
		t.Fatalf("unexpected cap stats dropped=%d stats=%+v", dropped, stats)
	}
}

func TestSpoolExpiresSevenDayHistory(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	queue, err := Open(t.TempDir()+"/spool.db", Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	old := protocol.Event{Timestamp: now.Add(-8 * 24 * time.Hour).Unix(), Type: "offline_start", Detail: map[string]any{}}
	fresh := protocol.Event{Timestamp: now.Unix(), Type: "offline_end", Detail: map[string]any{}}
	one, _ := NewRecord(KindEvent, old.Timestamp, old)
	two, _ := NewRecord(KindEvent, fresh.Timestamp, fresh)
	if _, err := queue.AppendMinute(context.Background(), []Record{one, two}); err != nil {
		t.Fatal(err)
	}
	stats, err := queue.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 1 || stats.Dropped != 1 {
		t.Fatalf("unexpected retention stats: %+v", stats)
	}
}

func TestSpoolExpiresSSIDInPendingBatch(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	queue, err := Open(t.TempDir()+"/spool.db", Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	link := protocol.LinkContext{Timestamp: now.Unix(), InterfaceType: "wifi", SSID: "Private WiFi"}
	record, _ := NewRecord(KindLinkContext, link.Timestamp, link)
	if _, err := queue.AppendMinute(context.Background(), []Record{record}); err != nil {
		t.Fatal(err)
	}
	pending, err := queue.NextBatch(context.Background(), "1.0.0", 1)
	if err != nil || pending == nil {
		t.Fatalf("pending batch = %#v, error = %v", pending, err)
	}

	now = now.Add(8 * 24 * time.Hour)
	fresh := protocol.Event{Timestamp: now.Unix(), Type: "retention_tick", Detail: map[string]any{}}
	freshRecord, _ := NewRecord(KindEvent, fresh.Timestamp, fresh)
	if _, err := queue.AppendMinute(context.Background(), []Record{freshRecord}); err != nil {
		t.Fatal(err)
	}
	stats, err := queue.Stats(context.Background())
	if err != nil || stats.Records != 1 || stats.Dropped != 1 {
		t.Fatalf("stats after pending expiry = %+v, error = %v", stats, err)
	}
	var oldBatches int
	if err := queue.db.QueryRow("SELECT COUNT(*) FROM pending_batch WHERE batch_id = ?", pending.Batch.BatchID).Scan(&oldBatches); err != nil || oldBatches != 0 {
		t.Fatalf("expired pending batches = %d, error = %v", oldBatches, err)
	}
}

func TestHistoryLimitReturnsNewestRowsInAscendingOrder(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	queue, err := Open(t.TempDir()+"/spool.db", Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	var records []Record
	for index := range 3 {
		timestamp := now.Add(time.Duration(index) * time.Minute).Unix()
		record, err := NewRecord(KindEvent, timestamp, protocol.Event{Timestamp: timestamp, Type: "sample", Detail: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if _, err := queue.AppendMinute(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	history, err := queue.History(context.Background(), now.Add(-time.Minute), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Timestamp != records[1].Timestamp || history[1].Timestamp != records[2].Timestamp {
		t.Fatalf("history timestamps = %+v", history)
	}
}

func TestPrunePreservesRecordsReservedByPendingBatch(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	queue, err := Open(t.TempDir()+"/spool.db", Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	records := make([]Record, 0, maxBatchRecords+1)
	for index := range maxBatchRecords + 1 {
		timestamp := now.Add(time.Duration(index) * time.Second).Unix()
		record, err := NewRecord(KindEvent, timestamp, protocol.Event{Timestamp: timestamp, Type: "sample", Detail: map[string]any{"index": index}})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if _, err := queue.AppendMinute(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	pending, err := queue.NextBatch(context.Background(), "1.0.0", 1)
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil || len(pending.RecordIDs) == 0 || len(pending.RecordIDs) >= len(records) {
		t.Fatalf("reserved record count = %d, total records = %d", len(pending.RecordIDs), len(records))
	}

	queue.maxPayloadBytes = 1
	extra, err := NewRecord(KindEvent, now.Add(time.Hour).Unix(), protocol.Event{Timestamp: now.Add(time.Hour).Unix(), Type: "extra", Detail: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.AppendMinute(context.Background(), []Record{extra}); err != nil {
		t.Fatal(err)
	}
	var reservedRows int
	if err := queue.db.QueryRow("SELECT COUNT(*) FROM records WHERE id IN ("+placeholders(len(pending.RecordIDs))+")", int64Args(pending.RecordIDs)...).Scan(&reservedRows); err != nil {
		t.Fatal(err)
	}
	if reservedRows != len(pending.RecordIDs) {
		t.Fatalf("reserved rows remaining = %d, want %d", reservedRows, len(pending.RecordIDs))
	}

	queue.maxPayloadBytes = DefaultMaxPayloadBytes
	if err := queue.Ack(context.Background(), pending.Batch.BatchID); err != nil {
		t.Fatal(err)
	}
	history, err := queue.History(context.Background(), now.Add(-time.Minute), 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != len(pending.RecordIDs) {
		t.Fatalf("history rows after ack = %d, want %d", len(history), len(pending.RecordIDs))
	}
}

func TestStatsSeparateLogicalPayloadBudgetFromPhysicalStorage(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	const limit = int64(8 << 10)
	queue, err := Open(t.TempDir()+"/spool.db", Options{
		MaxPayloadBytes: limit,
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	record, err := NewRecord(KindEvent, now.Unix(), protocol.Event{
		Timestamp: now.Unix(), Type: "sample", Detail: map[string]any{"payload": strings.Repeat("x", 256)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.AppendMinute(context.Background(), []Record{record}); err != nil {
		t.Fatal(err)
	}
	stats, err := queue.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.PayloadLimitBytes != limit || stats.PayloadBytes != int64(len(record.Payload)) {
		t.Fatalf("logical payload stats = %+v, record bytes = %d", stats, len(record.Payload))
	}
	if stats.StorageBytes <= stats.PayloadBytes {
		t.Fatalf("physical storage was not reported separately: %+v", stats)
	}
}

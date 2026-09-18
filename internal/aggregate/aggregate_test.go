package aggregate

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
)

func TestFlushBuildsMinuteSummary(t *testing.T) {
	t.Parallel()
	aggregator := New()
	start := time.Unix(1_800_000_005, 0)
	for index, rtt := range []float64{10, 20, 30, 40} {
		aggregator.AddSample(Sample{Timestamp: start.Add(time.Duration(index) * time.Second), Target: "anchor", RTTMS: rtt, Success: true, Fallback: "none"})
	}
	aggregator.AddSample(Sample{Timestamp: start.Add(5 * time.Second), Target: "anchor", Success: false})
	records, err := aggregator.Flush(start.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
	var metric protocol.MinuteMetric
	if err := json.Unmarshal(records[0].Payload, &metric); err != nil {
		t.Fatal(err)
	}
	if metric.Timestamp != start.Truncate(time.Minute).Unix() || metric.RTT.P50 != 20 || metric.RTT.P95 != 40 || metric.LossPct != 20 || metric.JitterMS != 10 || metric.Samples != 5 {
		t.Fatalf("unexpected summary: %+v", metric)
	}
	if len(aggregator.Live()) != 5 {
		t.Fatalf("live samples = %d", len(aggregator.Live()))
	}
}

func TestFlushIncludesChecksInSameTransactionSet(t *testing.T) {
	t.Parallel()
	aggregator := New()
	if err := aggregator.AddDNS(protocol.DNSCheck{Timestamp: 1_800_000_000, Resolver: "system", ColdMS: 1, WarmMS: 1, Hijack: false}); err != nil {
		t.Fatal(err)
	}
	if err := aggregator.AddHTTP(protocol.HTTPCheck{Timestamp: 1_800_000_000, Target: "https://example.com", Status: 204}); err != nil {
		t.Fatal(err)
	}
	records, err := aggregator.Flush(time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d", len(records))
	}
}

func TestSamplesFromDifferentMinutesUseDifferentBuckets(t *testing.T) {
	t.Parallel()
	aggregator := New()
	start := time.Unix(1_800_000_000, 0).UTC()
	aggregator.AddSample(Sample{Timestamp: start, Target: "anchor", RTTMS: 10, Success: true})
	aggregator.AddSample(Sample{Timestamp: start.Add(time.Minute), Target: "anchor", RTTMS: 20, Success: true})
	records, err := aggregator.Flush(start.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	for index, record := range records {
		var metric protocol.MinuteMetric
		if err := json.Unmarshal(record.Payload, &metric); err != nil {
			t.Fatal(err)
		}
		wantTimestamp := start.Add(time.Duration(index) * time.Minute).Unix()
		if metric.Timestamp != wantTimestamp || metric.Samples != 1 {
			t.Fatalf("metric %d = %+v, want timestamp %d and one sample", index, metric, wantTimestamp)
		}
	}
}

func TestMidMinuteFlushEmitsOneCompleteClosedMetric(t *testing.T) {
	t.Parallel()
	aggregator := New()
	minute := time.Unix(1_800_000_000, 0).UTC()
	aggregator.AddSample(Sample{Timestamp: minute.Add(5 * time.Second), Target: "anchor", RTTMS: 10, Success: true})

	records, err := aggregator.Flush(minute.Add(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("mid-minute records = %d, want 0", len(records))
	}

	aggregator.AddSample(Sample{Timestamp: minute.Add(40 * time.Second), Target: "anchor", RTTMS: 20, Success: true})
	records, err = aggregator.Flush(minute.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("closed-minute records = %d, want 1", len(records))
	}
	var metric protocol.MinuteMetric
	if err := json.Unmarshal(records[0].Payload, &metric); err != nil {
		t.Fatal(err)
	}
	if metric.Timestamp != minute.Unix() || metric.Samples != 2 {
		t.Fatalf("closed metric = %+v, want complete two-sample minute", metric)
	}

	records, err = aggregator.Flush(minute.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("second closed-minute flush emitted %d duplicate records", len(records))
	}
}

func TestShutdownDropsPartialMetricButPreservesPendingRecords(t *testing.T) {
	t.Parallel()
	aggregator := New()
	minute := time.Unix(1_800_000_000, 0).UTC()
	aggregator.AddSample(Sample{Timestamp: minute.Add(5 * time.Second), Target: "anchor", RTTMS: 10, Success: true})
	if err := aggregator.AddDNS(protocol.DNSCheck{Timestamp: minute.Unix(), Resolver: "system"}); err != nil {
		t.Fatal(err)
	}
	if err := aggregator.AddHTTP(protocol.HTTPCheck{Timestamp: minute.Unix(), Target: "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := aggregator.AddLink(protocol.LinkContext{Timestamp: minute.Unix(), InterfaceType: "ethernet"}); err != nil {
		t.Fatal(err)
	}
	if err := aggregator.AddSpeedTest(protocol.SpeedTest{Timestamp: minute.Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := aggregator.AddEvent(protocol.Event{Timestamp: minute.Unix(), Type: "shutdown-test"}); err != nil {
		t.Fatal(err)
	}

	records, err := aggregator.Shutdown(minute.Add(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 5 {
		t.Fatalf("shutdown records = %d, want all 5 pending non-metric records", len(records))
	}
	for _, record := range records {
		if record.Kind == "minute_metric" {
			t.Fatal("shutdown emitted a partial current-minute metric")
		}
	}
	records, err = aggregator.Flush(minute.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("post-shutdown flush emitted %d records from discarded partial bucket", len(records))
	}
}

func TestLateSampleCannotReopenFinalizedTargetMinute(t *testing.T) {
	t.Parallel()
	aggregator := New()
	minute := time.Unix(1_800_000_000, 0).UTC()
	aggregator.AddSample(Sample{Timestamp: minute.Add(5 * time.Second), Target: "anchor", RTTMS: 10, Success: true})
	if records, err := aggregator.Flush(minute.Add(time.Minute)); err != nil || len(records) != 1 {
		t.Fatalf("initial flush records=%d err=%v", len(records), err)
	}
	aggregator.AddSample(Sample{Timestamp: minute.Add(50 * time.Second), Target: "anchor", RTTMS: 20, Success: true})
	records, err := aggregator.Flush(minute.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("late sample reopened a sealed minute: %+v", records)
	}
}

func TestNewTargetAcceptsEpochAndPreEpochSamples(t *testing.T) {
	t.Parallel()
	aggregator := New()
	aggregator.AddSample(Sample{Timestamp: time.Unix(-1, 0), Target: "historical", RTTMS: 10, Success: true})
	records, err := aggregator.Flush(time.Unix(60, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("historical sample records = %d, want 1", len(records))
	}
}

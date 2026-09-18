// Package aggregate converts transient probe outcomes into one-minute rows.
package aggregate

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/internal/spool"
)

type (
	Sample struct {
		Timestamp   time.Time            `json:"timestamp"`
		Target      string               `json:"target"`
		RTTMS       float64              `json:"rtt_ms"`
		Success     bool                 `json:"success"`
		Fallback    string               `json:"fallback"`
		TargetClass protocol.TargetClass `json:"target_class,omitempty"`
		ProbeMethod protocol.ProbeMethod `json:"probe_method,omitempty"`
	}

	bucket struct {
		timestamp   time.Time
		target      string
		attempts    int
		values      []float64
		fallback    string
		targetClass protocol.TargetClass
		probeMethod protocol.ProbeMethod
	}

	bucketKey struct {
		target string
		minute int64
	}

	Aggregator struct {
		mu      sync.RWMutex
		buckets map[bucketKey]*bucket
		pending []spool.Record
		live    []Sample
		sealed  map[string]int64
	}
)

const (
	liveCapacity = 10 * 60
)

func New() *Aggregator {
	return &Aggregator{buckets: make(map[bucketKey]*bucket), pending: []spool.Record{}, live: make([]Sample, 0, liveCapacity), sealed: make(map[string]int64)}
}

func (a *Aggregator) AddSample(sample Sample) {
	a.mu.Lock()
	defer a.mu.Unlock()
	minute := sample.Timestamp.Truncate(time.Minute)
	key := bucketKey{target: sample.Target, minute: minute.Unix()}
	// Samples at or before a sealed minute are dropped by design: emitting a
	// second summary for a sealed minute would duplicate data the server
	// cannot recombine. After a backward wall-clock jump (NTP step, VM
	// restore) this loses samples until wall time passes the sealed minute —
	// a bounded loss taken deliberately over duplicate summaries.
	if sealedMinute, ok := a.sealed[sample.Target]; ok && key.minute <= sealedMinute {
		return
	}
	b := a.buckets[key]
	if b == nil {
		b = &bucket{timestamp: minute, target: sample.Target, values: make([]float64, 0, 60), fallback: sample.Fallback, targetClass: sample.TargetClass, probeMethod: sample.ProbeMethod}
		a.buckets[key] = b
	}
	b.attempts++
	if sample.Success && finiteBounded(sample.RTTMS) {
		b.values = append(b.values, sample.RTTMS)
	}
	if sample.Fallback != "" && sample.Fallback != "none" {
		b.fallback = sample.Fallback
	}
	if sample.TargetClass != "" {
		b.targetClass = sample.TargetClass
	}
	if sample.ProbeMethod != "" {
		b.probeMethod = sample.ProbeMethod
	}
	if len(a.live) == liveCapacity {
		copy(a.live, a.live[1:])
		a.live = a.live[:liveCapacity-1]
	}
	a.live = append(a.live, sample)
}

func (a *Aggregator) AddDNS(value protocol.DNSCheck) error {
	return a.addRecord(spool.KindDNSCheck, value.Timestamp, value)
}

func (a *Aggregator) AddHTTP(value protocol.HTTPCheck) error {
	return a.addRecord(spool.KindHTTPCheck, value.Timestamp, value)
}

func (a *Aggregator) AddLink(value protocol.LinkContext) error {
	return a.addRecord(spool.KindLinkContext, value.Timestamp, value)
}

func (a *Aggregator) AddSpeedTest(value protocol.SpeedTest) error {
	return a.addRecord(spool.KindSpeedTest, value.Timestamp, value)
}

func (a *Aggregator) AddEvent(value protocol.Event) error {
	if value.Detail == nil {
		value.Detail = map[string]any{}
	}
	return a.addRecord(spool.KindEvent, value.Timestamp, value)
}

func (a *Aggregator) addRecord(kind spool.Kind, timestamp int64, value any) error {
	record, err := spool.NewRecord(kind, timestamp, value)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.pending = append(a.pending, record)
	a.mu.Unlock()
	return nil
}

// Flush returns closed minute buckets and every pending non-metric record for
// one batched SQLite transaction. Buckets in the current minute remain open so
// forced upload, pause, and speed-test flushes cannot split their summaries.
func (a *Aggregator) Flush(now time.Time) ([]spool.Record, error) {
	return a.flush(now, false)
}

// Shutdown returns the same durable records as Flush, then deliberately drops
// partial current-minute buckets. A restarted process can therefore collect
// that minute again without uploading two summaries that cannot be recombined.
func (a *Aggregator) Shutdown(now time.Time) ([]spool.Record, error) {
	return a.flush(now, true)
}

// FlushPending returns non-metric records without closing any minute bucket.
// Service uses this while a latency producer is still able to publish samples
// timestamped before the current wall-clock minute.
func (a *Aggregator) FlushPending() []spool.Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	records := append([]spool.Record(nil), a.pending...)
	a.pending = []spool.Record{}
	return records
}

func (a *Aggregator) flush(now time.Time, discardOpen bool) ([]spool.Record, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	records := make([]spool.Record, 0, len(a.buckets)+len(a.pending))
	keys := make([]bucketKey, 0, len(a.buckets))
	cutoff := now.Truncate(time.Minute).Unix()
	for key := range a.buckets {
		if key.minute < cutoff {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].target == keys[j].target {
			return keys[i].minute < keys[j].minute
		}
		return keys[i].target < keys[j].target
	})
	for _, key := range keys {
		b := a.buckets[key]
		metric := summarize(b)
		record, err := spool.NewRecord(spool.KindMinuteMetric, metric.Timestamp, metric)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
		a.sealed[key.target] = max(a.sealed[key.target], key.minute)
	}
	records = append(records, a.pending...)
	if discardOpen {
		a.buckets = make(map[bucketKey]*bucket)
	} else {
		for _, key := range keys {
			delete(a.buckets, key)
		}
	}
	a.pending = []spool.Record{}
	return records, nil
}

func (a *Aggregator) Live() []Sample {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]Sample(nil), a.live...)
}

// Restore returns records to the pending set after a failed SQLite commit.
func (a *Aggregator) Restore(records []spool.Record) {
	a.mu.Lock()
	a.pending = append(records, a.pending...)
	a.mu.Unlock()
}

func (a *Aggregator) Reset() {
	a.mu.Lock()
	a.buckets = make(map[bucketKey]*bucket)
	a.pending = []spool.Record{}
	a.live = a.live[:0]
	a.sealed = make(map[string]int64)
	a.mu.Unlock()
}

func summarize(b *bucket) protocol.MinuteMetric {
	values := append([]float64(nil), b.values...)
	sort.Float64s(values)
	summary := protocol.RTTSummary{}
	if len(values) > 0 {
		summary.Min = values[0]
		summary.Max = values[len(values)-1]
		summary.P50 = percentile(values, 0.50)
		summary.P95 = percentile(values, 0.95)
	}
	var jitter float64
	for index := 1; index < len(b.values); index++ {
		jitter += math.Abs(b.values[index] - b.values[index-1])
	}
	if len(b.values) > 1 {
		jitter /= float64(len(b.values) - 1)
	}
	loss := 0.0
	if b.attempts > 0 {
		loss = 100 * float64(b.attempts-len(b.values)) / float64(b.attempts)
	}
	fallback := b.fallback
	if fallback == "" {
		fallback = "none"
	}
	return protocol.MinuteMetric{
		Timestamp: b.timestamp.Unix(), Target: b.target, RTT: summary,
		JitterMS: jitter, LossPct: loss, Samples: b.attempts, Fallback: fallback,
		TargetClass: b.targetClass, ProbeMethod: b.probeMethod,
	}
}

func percentile(sorted []float64, fraction float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := max(int(math.Ceil(fraction*float64(len(sorted))))-1, 0)
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func finiteBounded(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 60000
}

package metric

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestTelemetryChartBudgetIncludesDecodedStatistics(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-time.Hour)
	values := make([]float64, 256)
	for i := range values {
		values[i] = float64(i % 8)
	}
	if _, err := e.Accept(ctx, telemetryBatch("decoded-budget", at, values...), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	var raw string
	if err := e.s.db.QueryRow("SELECT stats FROM " + e.table("summary") + " LIMIT 1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stats telemetryStats
	if err := json.Unmarshal([]byte(raw), &stats); err != nil {
		t.Fatal(err)
	}
	sealed, err := stats.encodeSealed()
	if err != nil {
		t.Fatal(err)
	}
	budget := len(sealed) + len(`{"device":"0"}`) + 512
	if len(stats.Digest) <= budget {
		t.Fatal("fixture does not expand beyond the SQL-length budget")
	}
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute - time.Nanosecond)}, Interval: time.Minute, Aggregation: AggAvg, PreserveSeries: true}
	for _, encoded := range []string{raw, sealed} {
		if _, err = e.s.db.Exec("UPDATE "+e.table("summary")+" SET stats=?", encoded); err != nil {
			t.Fatal(err)
		}
		if _, err = e.ChartBatchBounded(ctx, []AggregateQuery{q}, now, 100, budget); !errors.Is(err, ErrTelemetryBackpressure) {
			t.Fatal("decoded allocation escaped its budget", err)
		}
		r, err := e.ChartBatchBounded(ctx, []AggregateQuery{q}, now, 100, 128<<10)
		if err != nil || len(r[0].Points) != 1 || r[0].Points[0].Value != 3.5 || r[0].Points[0].Count != 256 {
			t.Fatal("valid budget changed statistics", r, err)
		}
	}
}

func TestTelemetryChartBudgetMalformedFramesAndLegacyDigest(t *testing.T) {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithSingleSegment(true))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	oversized := `"TSC1:` + base64.StdEncoding.EncodeToString(encoder.EncodeAll(make([]byte, telemetryStatsLimit+1), nil)) + `"`
	for _, raw := range []string{`"TSC1:!"`, `"UNKNOWN:AAAA"`, `"TSC1:AAAA"`, oversized, `"TSC1:` + strings.Repeat("a", 2*telemetryStatsLimit) + `"`} {
		b := &telemetryChartBudget{points: 10, bytes: 8 << 20}
		ctx := context.WithValue(context.Background(), telemetryChartBudgetKey{}, b)
		var s telemetryStats
		if err = decodeChartStats(ctx, raw, &s); err == nil {
			t.Fatal("bad frame accepted")
		}
		if b.bytes != 8<<20 || len(s.Digest) != 0 {
			t.Fatal("failed decode retained allocation", b.bytes)
		}
	}
	d := NewTDigest(100)
	for i := 0; i < 400; i++ {
		d.Add(float64(i%7), 1)
	}
	compressed := compressTDigestBlob(d.encodeRaw())
	b := &telemetryChartBudget{points: 10, bytes: 8 << 20}
	ctx := context.WithValue(context.Background(), telemetryChartBudgetKey{}, b)
	got, err := decodeChartDigest(ctx, compressed)
	if err != nil || got.Count() != 400 || b.bytes != 8<<20-(128+cap(got.centroids)*16) {
		t.Fatal(got, b.bytes, err)
	}
	// Forge a valid-looking tiny header followed by a huge deflate tail. The
	// decoder rejects immediately after the declared centroids, before expansion.
	header := d.encodeRaw()[:39]
	binary.LittleEndian.PutUint32(header[35:], 0)
	var packed bytes.Buffer
	w, _ := flate.NewWriter(&packed, flate.BestSpeed)
	w.Write(header)
	w.Write(make([]byte, 8<<20))
	w.Close()
	forged := append([]byte{'T', 'Z', tdigestVersion}, packed.Bytes()...)
	if _, err = decodeChartDigest(ctx, forged); err == nil {
		t.Fatal("undeclared digest tail accepted")
	}
	forged[2]++
	if _, err = chartDigestBytes(forged); err == nil {
		t.Fatal("unknown digest version accepted")
	}
}

func TestTelemetryChartSharedBudgetAndHotQuantiles(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-time.Minute)
	if _, err := e.Accept(ctx, telemetryBatch("summary", at, 1, 2, 3), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := e.Accept(ctx, telemetryBatch("hot", at.Add(3*time.Millisecond), 100, 101, 102), now); err != nil {
		t.Fatal(err)
	}
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute - time.Nanosecond)}, Interval: time.Minute, Aggregation: AggP50, PreserveSeries: true}
	r, err := e.Chart(ctx, q, now)
	if err != nil || len(r.Points) != 1 || r.Points[0].Count != 6 || r.Points[0].Value != 51.5 {
		t.Fatal("summary/hot quantile lost population", r, err)
	}
	b := &telemetryChartBudget{points: 10, bytes: 1}
	ctx = context.WithValue(ctx, telemetryChartBudgetKey{}, b)
	q.MetricName = sqliteMergedPingLatencyMetric
	// An explicit caller budget must never be silently replaced by a default.
	if defaultChartBudget(ctx).Value(telemetryChartBudgetKey{}) != b {
		t.Fatal("caller budget replaced")
	}
	_, _ = e.PingSummary(ctx, q, now)
	if b.bytes != 1 {
		t.Fatal("empty Ping changed budget")
	}
}

// Opt-in fresh-process memory measurement. The summaries are real production
// encodings; thousands of independent identities make retained decoded data
// dominate compressed SQL bytes. Both chart and summary-CSV budgets are tested.
func TestTelemetryChartHighCardinalityResourceBudget(t *testing.T) {
	output := os.Getenv("KOMARI_TELEMETRY_CHART_MEMORY_OUTPUT")
	if output == "" {
		t.Skip("explicit finite resource experiment")
	}
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-time.Hour)
	var stats telemetryStats
	for i := 0; i < 256; i++ {
		stats.add(Point{MetricName: "cpu", Timestamp: at.Add(time.Second), Value: float64(i % 8)}, 1, i)
	}
	sealed, err := stats.encodeSealed()
	if err != nil {
		t.Fatal(err)
	}
	// Obtain an ordinary online ownership row from production ingestion, then
	// populate independently named summary series in one isolated transaction.
	if _, err = e.Accept(ctx, telemetryBatch("resource", at, 1), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	var segment string
	if err = e.s.db.QueryRow("SELECT segment_id FROM " + e.table("summary") + " LIMIT 1").Scan(&segment); err != nil {
		t.Fatal(err)
	}
	tx, err := e.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"summary", "members", "hot", "series"} {
		if _, err = tx.Exec("DELETE FROM " + e.table(table)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4096; i++ {
		sid := fmt.Sprintf("resource-%d", i)
		tags := fmt.Sprintf(`{"device":"%d"}`, i)
		if _, err = tx.Exec("INSERT INTO "+e.table("series")+" (id,metric,entity,tags) VALUES (?,'cpu','node',?)", sid, tags); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec("INSERT INTO "+e.table("members")+" (segment_id,series_id,first_observed,last_observed,points) VALUES (?,?,?,?,256)", segment, sid, at.UnixNano(), at.Add(time.Second).UnixNano()); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec("INSERT INTO "+e.table("summary")+" (segment_id,series_id,resolution,bucket,stats) VALUES (?,?,?,?,?)", segment, sid, int64(time.Minute), at.UnixNano(), sealed); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Exercise a real distribution consumer. Scalar mean charts deliberately
	// discard digests now and no longer have the same resident-memory floor.
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute - time.Nanosecond)}, Interval: time.Minute, Aggregation: AggP50, PreserveSeries: true}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err = e.ChartBatchBounded(ctx, []AggregateQuery{q}, now, 20000, 8<<20); !errors.Is(err, ErrTelemetryBackpressure) {
		t.Fatal("8MiB summary export escaped", err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	r, err := e.ChartBatchBounded(ctx, []AggregateQuery{q}, now, 20000, 32<<20)
	if err != nil || len(r[0].Points) != 4096 {
		t.Fatal("valid full query rejected", err)
	}
	for _, p := range r[0].Points {
		if p.Value != 3.5 || p.Count != 256 {
			t.Fatal(p)
		}
	}
	status, _ := os.ReadFile("/proc/self/status")
	report := map[string]any{"series": 4096, "encoded_cell_bytes": len(sealed), "retained_digest_bytes": cap(stats.sketch.encodeRaw()), "small_budget": 8 << 20, "large_budget": 32 << 20, "small_query_heap_alloc_delta": int64(after.HeapAlloc) - int64(before.HeapAlloc), "small_query_total_alloc_delta": after.TotalAlloc - before.TotalAlloc, "process_status": string(status)}
	if group, readErr := os.ReadFile("/proc/self/cgroup"); readErr == nil {
		for _, line := range strings.Split(string(group), "\n") {
			if strings.HasPrefix(line, "0::") {
				base := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "0::"))
				for _, file := range []string{"cpu.max", "cpu.stat", "memory.max", "memory.peak", "memory.swap.max"} {
					if value, err := os.ReadFile(filepath.Join(base, file)); err == nil {
						report[file] = strings.TrimSpace(string(value))
					}
				}
			}
		}
	}
	if os.Getenv("KOMARI_TELEMETRY_REQUIRE_CHART_CGROUP") == "1" {
		if report["cpu.max"] != "10000 100000" || report["memory.max"] != "134217728" || report["memory.swap.max"] != "0" {
			t.Fatal("resource controls not enforced", report)
		}
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(output, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log(string(b))
}

func TestTelemetryChartDecodeBudgetRejectsBeforeExpansionAndRefundsScratch(t *testing.T) {
	var stats telemetryStats
	for i := 0; i < 256; i++ {
		stats.add(Point{MetricName: "cpu", Value: float64(i % 8), Timestamp: time.Unix(100, 0)}, 1, i)
	}
	sealed, err := stats.encodeSealed()
	if err != nil {
		t.Fatal(err)
	}
	b := &telemetryChartBudget{points: 1, bytes: 64 << 10}
	ctx := context.WithValue(context.Background(), telemetryChartBudgetKey{}, b)
	var decoded telemetryStats
	before := b.bytes
	if err = decodeChartStats(ctx, sealed, &decoded); err != nil {
		t.Fatal(err)
	}
	if before-b.bytes != cap(decoded.Digest) {
		t.Fatal("scratch leaked or retained digest uncharged", before-b.bytes, cap(decoded.Digest))
	}
	var merged telemetryStats
	before = b.bytes
	if err = mergeChartStats(ctx, &merged, decoded); err != nil {
		t.Fatal(err)
	}
	if before-b.bytes != chartStatsBytes(&merged) {
		t.Fatal("merge scratch leaked", before-b.bytes, chartStatsBytes(&merged))
	}
	b.bytes = 1
	beforeStats := merged
	if err = mergeChartStats(ctx, &merged, decoded); !errors.Is(err, ErrTelemetryBackpressure) || merged.N != beforeStats.N {
		t.Fatal("failed merge modified the result", err)
	}
}

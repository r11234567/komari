package metric

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"
	"time"
)

func scalarQuery(at time.Time, span, step time.Duration, name string) AggregateQuery {
	return AggregateQuery{Query: Query{MetricName: name, EntityID: "node", Start: at, End: at.Add(span - time.Nanosecond)}, Interval: step, Aggregation: AggAvg, PreserveSeries: true}
}

func scalarDrain(t *testing.T, e *Telemetry, now time.Time) {
	t.Helper()
	ctx := context.Background()
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, now.Add(e.cfg.SegmentDuration)); err != nil {
		t.Fatal(err)
	}
	for {
		ok, err := e.ArchiveNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
	}
	for {
		n, err := e.DeleteArchivedChunk(ctx, now, 512)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
}

func TestScalarHandoffRetiresRawAndReopensCharts(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	at := now.Add(-10 * time.Minute).Truncate(time.Minute)
	if _, err := e.Accept(ctx, telemetryBatch("handoff", at, 2, 8, 20), now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DeleteArchivedChunk(ctx, now, 512); err != nil {
		t.Fatal(err)
	}
	if telemetryCount(t, e, "hot") != 3 {
		t.Fatal("deleted before archive")
	}
	scalarDrain(t, e, now)
	if telemetryCount(t, e, "hot") != 3 || telemetryCount(t, e, "summary") != 0 {
		t.Fatal("live display rows lost or legacy summary retained")
	}
	q := scalarQuery(at, time.Minute, time.Minute, "cpu")
	r, err := e.Chart(ctx, q, now)
	if err != nil || len(r.Points) != 1 || r.Points[0].Value != 10 || r.Points[0].Count != 3 || len(r.Sources) == 0 || len(r.Gaps) != 0 {
		t.Fatal(r, err)
	}
	// Chart has no dependency on the cold archive being mounted.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := e.cfg
	missing := cfg.ArchiveDirectory + "-offline"
	if err := os.Rename(cfg.ArchiveDirectory, missing); err != nil {
		t.Fatal(err)
	}
	reopened, err := s.OpenTelemetry(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r, err = reopened.Chart(ctx, q, now)
	if err != nil || len(r.Points) != 1 || r.Points[0].Count != 3 {
		t.Fatal(r, err)
	}
}

func TestScalarAgingIsAtomicAdditiveAndIdempotent(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	at := now.Add(-20 * time.Minute).Truncate(time.Minute)
	if _, err := e.Accept(ctx, telemetryBatch("base", at, 2, 8), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	failure := errors.New("injected handoff failure")
	e.cfg.Fault = func(stage string) error {
		if stage == "tier-parent-before-handoff" {
			return failure
		}
		return nil
	}
	if _, err := e.PromoteSummariesChunk(ctx, now.Add(3*time.Hour), 1); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	q := scalarQuery(at, time.Minute, time.Minute, "cpu")
	r, err := e.Chart(ctx, q, now)
	if err != nil || len(r.Points) != 1 || r.Points[0].Count != 2 {
		t.Fatal("failed promotion mutated input", r, err)
	}
	e.cfg.Fault = nil
	for {
		n, err := e.PromoteSummariesChunk(ctx, now.Add(3*time.Hour), 1)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if _, err := e.Accept(ctx, telemetryBatch("late", at.Add(5*time.Second), 20), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	for pass := 0; pass < 2; pass++ {
		for {
			n, err := e.PromoteSummariesChunk(ctx, now.Add(3*time.Hour), 1)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
		}
	}
	r, err = e.Chart(ctx, q, now)
	if err != nil || len(r.Points) != 1 || r.Points[0].Count != 3 || r.Points[0].Value != 10 {
		t.Fatal("promotion replay/lateness changed population", r, err)
	}
	if telemetryCount(t, e, "rollup") != 1 || telemetryCount(t, e, "summary") != 0 {
		t.Fatal("permanent ancestor copies")
	}
}

func TestScalarWindowsMatchObservationOracle(t *testing.T) {
	for _, hours := range []int{1, 6, 12, 24, 168, 360, 1440, 4320, 8760} {
		t.Run(fmt.Sprint(hours), func(t *testing.T) {
			_, e := telemetryFixture(t, "")
			e.cfg.MaxLateness = 400 * 24 * time.Hour
			ctx := context.Background()
			now := time.Now().UTC()
			span := time.Duration(hours) * time.Hour
			step := TelemetryChartInterval(span, 600)
			start := alignTime(now.Add(-span+2*time.Minute), step).Add(step)
			var points []Point
			for at := start; at.Before(now); at = at.Add(step) {
				points = append(points, Point{MetricName: "cpu", EntityID: "node", Timestamp: at, Value: float64(len(points) % 97)})
			}
			for offset := 0; offset < len(points); offset += 128 {
				if _, err := e.Accept(ctx, TelemetryBatch{Source: "node", Identity: fmt.Sprint(offset), Points: points[offset:min(offset+128, len(points))]}, now); err != nil {
					t.Fatal(err)
				}
			}
			summarizeAll(t, e)
			for {
				n, err := e.PromoteSummariesChunk(ctx, now, 128)
				if err != nil {
					t.Fatal(err)
				}
				if n == 0 {
					break
				}
			}
			q := scalarQuery(start, now.Sub(start)+time.Nanosecond, step, "cpu")
			want, err := AggregatePoints(points, q)
			if err != nil {
				t.Fatal(err)
			}
			got, err := e.Chart(ctx, q, now)
			if err != nil || len(got.Points) != len(want) || len(got.Gaps) != 0 {
				t.Fatalf("got %d want %d gaps=%d err=%v", len(got.Points), len(want), len(got.Gaps), err)
			}
			for i, p := range got.Points {
				if p.Count != want[i].Count || math.Abs(p.Value-want[i].Value) > 1e-9 || !p.Bucket.Equal(want[i].Bucket) {
					t.Fatal(p, want[i])
				}
			}
		})
	}
}

func TestScalarPingCountsAndUnsupportedStatistics(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	at := now.Add(-3 * time.Hour).Truncate(time.Minute)
	b := TelemetryBatch{Source: "node", Identity: "ping"}
	for i, v := range []float64{-1, 0, 20, -1} {
		b.Points = append(b.Points, Point{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node", Timestamp: at.Add(time.Duration(i) * time.Second), Value: v, Tags: map[string]string{"task_id": "1"}})
	}
	if _, err := e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	q := scalarQuery(at, time.Minute, time.Minute, sqliteMergedPingLatencyMetric)
	s, err := e.PingSummary(ctx, q, now)
	if err != nil || len(s.Avg) != 1 || s.Avg[0].Value != 10 || s.Avg[0].Count != 2 || len(s.Loss) != 1 || s.Loss[0].Count != 4 || s.Loss[0].Value != 0.5 {
		t.Fatal(s, err)
	}
	if len(s.P50)+len(s.P99)+len(s.StdDev) != 0 {
		t.Fatal("invented historical distribution")
	}
	q.MetricName = sqliteVirtualPingLossMetric
	r, err := e.Chart(ctx, q, now)
	if err != nil || len(r.Points) != 1 || r.Points[0].Value != 0.5 || r.Points[0].Count != 4 {
		t.Fatal(r, err)
	}
	q.MetricName = sqliteMergedPingLatencyMetric
	for _, a := range []Aggregation{AggP50, AggP95, AggP99, AggStdDev, AggFirst} {
		q.Aggregation = a
		if _, err := e.Chart(ctx, q, now); !errors.Is(err, ErrInvalidArgument) {
			t.Fatal(a, err)
		}
	}
}

func TestScalarMissingPopulationSuppressesOnlyAffectedSeriesBucket(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	at := now.Add(-3 * 24 * time.Hour).Truncate(time.Hour)
	e.cfg.MaxLateness = 7 * 24 * time.Hour
	b := telemetryBatch("coarse", at, 2)
	b.Points = append(b.Points, Point{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(20 * time.Minute), Value: 8, Tags: b.Points[0].Tags})
	if _, err := e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := e.Accept(ctx, telemetryBatch("fine-delta", at.Add(5*time.Minute), 100), now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Accept(ctx, TelemetryBatch{Source: "other", Identity: "other", Points: []Point{{MetricName: "cpu", EntityID: "other", Timestamp: at.Add(5 * time.Minute), Value: 0}}}, now); err != nil {
		t.Fatal(err)
	}
	q := scalarQuery(at, time.Hour, 10*time.Minute, "cpu")
	q.EntityID = ""
	r, err := e.Chart(ctx, q, now)
	if err != nil || len(r.Gaps) == 0 || len(r.Points) != 1 || r.Points[0].EntityID != "other" {
		t.Fatal("published partial total", r, err)
	}
}

func TestScalarArchiveSelectsMachineBeforeDecoding(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	at := now.Add(-10 * time.Minute).Truncate(time.Minute)
	for _, node := range []string{"node", "other"} {
		for batch := 0; batch < 16; batch++ {
			b := TelemetryBatch{Source: node, Identity: fmt.Sprint(batch)}
			for i := 0; i < 256; i++ {
				b.Points = append(b.Points, Point{MetricName: "cpu", EntityID: node, Timestamp: at.Add(time.Duration(batch*256+i) * time.Millisecond), Value: float64(i)})
			}
			if _, err := e.Accept(ctx, b, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	scalarDrain(t, e, now)
	snapshot, err := e.FreezeExport(WithTelemetryEntities(ctx, []string{"node"}), []Query{{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute)}}, t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Units) == 0 || len(snapshot.Units) >= 4096/128 {
		t.Fatalf("tiny point-count frames: %d", len(snapshot.Units))
	}
	// Re-freeze into the directory retained for actual visitor traversal.
	dir := t.TempDir()
	snapshot, err = e.FreezeExport(WithTelemetryEntities(ctx, []string{"node"}), snapshot.Queries, dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	var decoded, written int64
	for i, u := range snapshot.Units {
		if u.Frame == nil || u.Frame.Version != 3 || u.Frame.Entity != "node" || u.Frame.Hash != "" {
			t.Fatal("missing coarse selection", u)
		}
		err = VisitExportInputProgress(WithTelemetryEntities(ctx, []string{"node"}), dir, snapshot, i, func(n, _ int64) { decoded += n }, func(o ExportObservation) error {
			if o.Point.EntityID != "node" {
				t.Fatal(o)
			}
			written++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if decoded != 4096 || written != 4096 {
		t.Fatal("single-node export decoded another machine", decoded, written)
	}
}

func TestScalarMigrationFencesAndResumesOldStatistics(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	at := now.Add(-3 * time.Hour).Truncate(time.Minute)
	if _, err := e.Accept(ctx, telemetryBatch("old", at, 2, 8), now); err != nil {
		t.Fatal(err)
	}
	// Build a real v7 alternate representation, including its old envelope.
	e.scalar = false
	summarizeAll(t, e)
	if _, err := s.db.Exec("UPDATE " + e.table("progress") + " SET value=7 WHERE task='schema_version'"); err != nil {
		t.Fatal(err)
	}
	cfg := e.cfg
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("conversion interrupted")
	cfg.Fault = func(stage string) error {
		if stage == "scalar-migration-before-commit" {
			return failure
		}
		return nil
	}
	if _, err := s.OpenTelemetry(ctx, cfg); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow("SELECT value FROM " + e.table("progress") + " WHERE task='schema_version'").Scan(&version); err != nil || version != -10 {
		t.Fatal(version, err)
	}
	cfg.Fault = nil
	target, err := s.OpenTelemetry(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	r, err := target.Chart(ctx, scalarQuery(at, time.Minute, time.Minute, "cpu"), now)
	if err != nil || len(r.Points) != 1 || r.Points[0].Count != 2 || r.Points[0].Value != 5 {
		t.Fatal(r, err)
	}
	if telemetryCount(t, target, "summary") != 0 || telemetryCount(t, target, "rollup") != 1 {
		t.Fatal("legacy envelopes survived")
	}
}

func TestScalarMigrationMissingRawAndErase(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	var cells []telemetryMigrationCell
	for i := 0; i < 3; i++ {
		ns := at.Add(time.Duration(i) * time.Minute).UnixNano()
		cells = append(cells, telemetryMigrationCell{series: telemetryMigrationKey("cpu\x00node\x00{}"), tags: "{}", bucket: ns, resolution: time.Minute, stats: telemetryStats{N: 2, Attempts: 2, Sum: 10, Min: 2, Max: 8, Last: 8, FirstNano: ns, LastNano: ns + int64(time.Second)}})
	}
	if err := e.commitLegacyCells(ctx, "missing", "cpu", "node", cells); err != nil {
		t.Fatal(err)
	}
	if telemetryCount(t, e, "raw_gaps") != 1 {
		t.Fatal("uncoalesced raw absence")
	}
	_, err := e.FreezeExport(WithTelemetryEntities(ctx, []string{"node"}), []Query{{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(3 * time.Minute)}}, t.TempDir(), 1<<20)
	if !errors.Is(err, ErrTelemetryCoverage) {
		t.Fatal("successful incomplete raw CSV", err)
	}
	if _, err := e.EraseSeries(ctx, Query{MetricName: "cpu", EntityID: "node"}); err != nil {
		t.Fatal(err)
	}
	if telemetryCount(t, e, "rollup")+telemetryCount(t, e, "raw_gaps") != 0 {
		t.Fatal("erasure left scalar history visible")
	}
}

func TestScalarSchemaHasNoCompressedStatistics(t *testing.T) {
	_, e := telemetryFixture(t, "")
	rows, err := e.s.db.Query("PRAGMA table_info(" + e.table("rollup") + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "stats" || name == "digest" || typ == "BLOB" || typ == "TEXT" {
			t.Fatal(name, typ)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Retired settings never become an implicit day of live raw input.
	b, _ := json.Marshal(e.cfg)
	if len(b) == 0 || e.cfg.RawRetention != 0 || e.cfg.MaxHotBytes > 16<<20 || e.cfg.SegmentBytes > 4<<20 {
		t.Fatal(string(b))
	}
}

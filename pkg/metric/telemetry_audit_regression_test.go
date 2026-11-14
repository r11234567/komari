package metric

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestTelemetryExpiredArchiveRejectsRawBeforeCatalogGC(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	e.cfg.MaxLateness = 400 * 24 * time.Hour
	if err := s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 365}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Accept(ctx, telemetryBatch("old", at, 1, 2), at); err != nil {
		t.Fatal(err)
	}
	lifecycleArchiveAll(t, e, at.Add(time.Hour))
	if ok, err := e.ExpireArchiveNext(ctx, at.Add(40*24*time.Hour), 30*24*time.Hour); err != nil || !ok {
		t.Fatal(ok, err)
	}
	queries := []Query{{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute)}}
	scoped := WithTelemetryEntities(ctx, []string{"node"})
	if _, err := e.ExportRawBatch(scoped, queries); !errors.Is(err, ErrTelemetryCoverage) {
		t.Fatal("expired raw became an empty export", err)
	}
	if _, err := e.FreezeExport(scoped, queries, t.TempDir(), 4<<20); !errors.Is(err, ErrTelemetryCoverage) {
		t.Fatal("expired raw became an empty snapshot", err)
	}
	// Pre-upgrade expired owners did not publish a floor. Both readers must
	// reject them before the upgraded catalog collector has run.
	if _, err := s.db.Exec("DELETE FROM " + e.table("progress") + " WHERE task='raw_history_floor'"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ExportRawBatch(scoped, queries); !errors.Is(err, ErrTelemetryCoverage) {
		t.Fatal("old expired owner", err)
	}
	if _, err := e.FreezeExport(scoped, queries, t.TempDir(), 4<<20); !errors.Is(err, ErrTelemetryCoverage) {
		t.Fatal("old expired snapshot", err)
	}
	auditDrainCatalog(t, e, at.Add(40*24*time.Hour))
	if n := telemetryCount(t, e, "segments"); n != 0 {
		t.Fatal("expired owner kept for chart retention", n)
	}
	if _, err := e.ExportRawBatch(scoped, queries); !errors.Is(err, ErrTelemetryCoverage) {
		t.Fatal("GC lost expiry", err)
	}
	r, err := e.Chart(ctx, AggregateQuery{Query: queries[0], Interval: time.Minute, Aggregation: AggSum, PreserveSeries: true}, at.Add(time.Hour))
	if err != nil || len(r.Points) != 1 || r.Points[0].Value != 3 {
		t.Fatal("raw expiry damaged chart", r, err)
	}
}

func TestTelemetryPresetPrecisionSurvivesRequestLatency(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct{ span, resolution time.Duration }{{time.Hour, 10 * time.Second}, {7 * 24 * time.Hour, 30 * time.Minute}, {15 * 24 * time.Hour, time.Hour}} {
		if got := telemetryResolution(now.Sub(now.Add(-tc.span - time.Second))); got != tc.resolution {
			t.Fatalf("%s + request latency: got %s want %s", tc.span, got, tc.resolution)
		}
	}
}

func auditDrainCatalog(t *testing.T, e *Telemetry, now time.Time) {
	t.Helper()
	for {
		n, err := e.PruneReceipts(context.Background(), now, 512)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	for {
		n, err := e.PruneCatalogChunk(context.Background(), now, 512)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
}

func TestTelemetryCatalogCoalescesWithoutRewritingOrLosingPinnedExports(t *testing.T) {
	dir := t.TempDir()
	s, e := telemetryFixture(t, dir)
	ctx := WithTelemetryEntities(context.Background(), []string{"node"})
	at := time.Now().UTC().Add(-24 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour)
	for i := 0; i < 4; i++ {
		atPoint := at.Add(time.Duration(i) * 20 * time.Minute)
		if _, err := e.Accept(ctx, telemetryBatch(fmt.Sprint(i), atPoint, float64(i), float64(i+10)), atPoint); err != nil {
			t.Fatal(err)
		}
		lifecycleArchiveAll(t, e, atPoint.Add(time.Minute))
	}
	q := []Query{{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(2 * time.Hour)}}
	want, err := e.ExportRawBatch(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	frozenDir := t.TempDir()
	frozen, err := e.FreezeExport(ctx, q, frozenDir, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	var seg telemetrySegment
	var id string
	if err = s.db.QueryRow("SELECT id FROM " + e.table("segments") + " ORDER BY archive_offset LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	seg, err = e.segment(ctx, s.db, id)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dir + "/archive/" + e.segmentPath(seg))
	if err != nil {
		t.Fatal(err)
	}
	// A catalog lease protects the identity seen by an active reader.
	if _, err = s.db.Exec(e.bind("INSERT INTO "+e.table("leases")+" (id,segment_id,expires) SELECT ?,id,? FROM "+e.table("segments")), "audit-pin", time.Now().Add(time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	auditDrainCatalog(t, e, time.Now().UTC())
	if n := telemetryCount(t, e, "segments"); n != 4 {
		t.Fatal("merged leased owners", n)
	}
	// Pre-v11 archives have no cached index limits. Initialize them once as
	// part of the same bounded, rollback-safe directory maintenance.
	if _, err = s.db.Exec("UPDATE " + e.table("segments") + " SET archive_frames=0,archive_parts=0,archive_index_bytes=0"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("DELETE FROM " + e.table("leases") + " WHERE id='audit-pin'"); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("merge rollback")
	e.cfg.Fault = func(stage string) error {
		if stage == "catalog-merge-before-commit" {
			return failure
		}
		return nil
	}
	if _, err = e.PruneCatalogChunk(ctx, time.Now(), 512); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if n := telemetryCount(t, e, "segments"); n != 4 {
		t.Fatal("merge was not atomic", n)
	}
	e.cfg.Fault = nil
	auditDrainCatalog(t, e, time.Now().UTC())
	if n := telemetryCount(t, e, "segments"); n != 1 {
		t.Fatal("handoff owners remained", n)
	}
	if n := telemetryCount(t, e, "members"); n != 1 {
		t.Fatal("repeated member catalog", n)
	}
	after, err := os.ReadFile(dir + "/archive/" + e.segmentPath(seg))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("merge rewrote immutable archive", err)
	}
	got, err := e.ExportRawBatch(ctx, q)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("coalesced export population", err, len(got))
	}
	mergedDir := t.TempDir()
	mergedSnapshot, err := e.FreezeExport(ctx, q, mergedDir, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	var mergedValues []float64
	for i := range mergedSnapshot.Units {
		if err = VisitExportInput(ctx, mergedDir, mergedSnapshot, i, func(p ExportObservation) error { mergedValues = append(mergedValues, p.Point.Value); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(mergedValues) != 8 {
		t.Fatal("new frozen composite lost records", mergedValues)
	}
	var pinned []float64
	for i := range frozen.Units {
		if err = VisitExportInput(ctx, frozenDir, frozen, i, func(p ExportObservation) error { pinned = append(pinned, p.Point.Value); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(pinned) != 8 {
		t.Fatal("frozen export changed", len(pinned))
	}
	e.Close()
	s.Close()
	s, e = telemetryFixture(t, dir)
	got, err = e.ExportRawBatch(ctx, q)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("reopened composite", err)
	}
	// Appending after coalescence must preserve the published prefix too.
	later := at.Add(80 * time.Minute)
	if _, err = e.Accept(ctx, telemetryBatch("later", later, 99), later); err != nil {
		t.Fatal(err)
	}
	lifecycleArchiveAll(t, e, later.Add(time.Minute))
	auditDrainCatalog(t, e, time.Now())
	got, err = e.ExportRawBatch(ctx, q)
	if err != nil || len(got[0]) != 9 {
		t.Fatal("append after merge", err)
	}
}

func TestTelemetryRollingPresetPlanHasCompleteSingleRequestBounds(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second).Add(137 * time.Millisecond)
	for _, tc := range []struct{ span, step time.Duration }{{time.Hour, 10 * time.Second}, {4 * time.Hour, time.Minute}, {24 * time.Hour, 5 * time.Minute}, {7 * 24 * time.Hour, 30 * time.Minute}, {15 * 24 * time.Hour, time.Hour}, {30 * 24 * time.Hour, 2 * time.Hour}} {
		t.Run(tc.span.String(), func(t *testing.T) {
			start := now.Add(-tc.span - time.Second)
			// An older parent overlaps the requested bucket in geometry only;
			// all its actual observations precede the request.
			at := alignTime(start, tc.step).Add(-tc.step)
			seedScalarCell(t, e, at, tc.step, 1, 2)
			q, step, err := e.PlanChart(ctx, Query{MetricName: "cpu", EntityID: "node", Start: start, End: now}, now, 600, 365)
			if err != nil || step != tc.step {
				t.Fatal("preset precision", step, tc.step, err)
			}
			if q.Start.Before(start) || q.Start.Sub(start) >= step || !alignTime(q.Start, step).Equal(q.Start) {
				t.Fatal("effective left boundary", q)
			}
			seedScalarCell(t, e, q.Start, tc.step, 4, 6)
			r, err := e.Chart(ctx, AggregateQuery{Query: q, Interval: step, Aggregation: AggSum, PreserveSeries: true}, now)
			if err != nil || len(r.Gaps) != 0 || len(r.Points) != 1 || r.Points[0].Value != 10 {
				t.Fatal("single request boundary", r, err)
			}
			if _, err = e.s.db.Exec("DELETE FROM " + e.table("rollup")); err != nil {
				t.Fatal(err)
			}
			if _, err = e.s.db.Exec("DELETE FROM " + e.table("rollup_due")); err != nil {
				t.Fatal(err)
			}
		})
	}
	q, step, err := e.PlanChart(ctx, Query{MetricName: "cpu", Start: now.Add(-365 * 24 * time.Hour), End: now}, now, 600, 1)
	if err != nil || step != 5*time.Minute || q.Start.Before(now.Add(-24*time.Hour)) {
		t.Fatal("precision selected before retention", q, step, err)
	}
}

func TestTelemetryMinuteTierRetiresAfterLastPreset(t *testing.T) {
	_, e := telemetryFixture(t, "")
	at := time.Now().UTC().Truncate(time.Minute)
	seedScalarCell(t, e, at, time.Minute, 1, 4)
	promoteTelemetryAll(t, e, at.Add(12*time.Hour+time.Second))
	var resolution int64
	if err := e.s.db.QueryRow("SELECT MIN(resolution) FROM " + e.table("rollup")).Scan(&resolution); err != nil || resolution != 60 {
		t.Fatal("12h source retired early", resolution, err)
	}
	promoteTelemetryAll(t, e, at.Add(12*time.Hour+telemetryTierGrace+time.Minute))
	if err := e.s.db.QueryRow("SELECT MIN(resolution) FROM " + e.table("rollup")).Scan(&resolution); err != nil || resolution != 300 {
		t.Fatal("unused 1m source remains", resolution, err)
	}
}

func TestTelemetryChartIntervalIgnoresUnobservedCoarseOverlap(t *testing.T) {
	_, e := telemetryFixture(t, "")
	now := time.Now().UTC().Truncate(time.Hour).Add(23*time.Minute + time.Second)
	start := now.Add(-7 * 24 * time.Hour)
	// The hour bucket geometrically straddles start, but its complete
	// population was observed before it. It cannot coarsen a rolling 7d view.
	seedScalarCell(t, e, alignTime(start, time.Hour), time.Hour, 1, 2)
	q, step, err := e.PlanChart(context.Background(), Query{MetricName: "cpu", EntityID: "node", Start: start, End: now}, now, 600, 365)
	if err != nil || step != 30*time.Minute {
		t.Fatal("unrelated parent coarsened window", q, step, err)
	}
}

// Reproduce the audit's catalog turnover (6 nodes x 29 series x 72 handoffs
// per day x 30 days). Each handoff traverses the real writer, archive, deletion
// and catalog paths. Sampling density is deliberately low: this isolates the
// metadata growth from raw compression and chart-population benchmarks.
func TestTelemetryThirtyDayCatalogTurnover(t *testing.T) {
	if os.Getenv("KOMARI_TELEMETRY_CATALOG_LONG") != "1" {
		t.Skip("opt-in 30-day catalog lifecycle")
	}
	dir := t.TempDir()
	if output := os.Getenv("KOMARI_TELEMETRY_CATALOG_OUTPUT"); output != "" {
		if err := os.MkdirAll(output, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(output, "metrics.db")); !os.IsNotExist(err) {
			t.Fatal("catalog output must be fresh", err)
		}
		dir = output
	}
	_, e := telemetryFixture(t, dir)
	ctx := context.Background()
	e.cfg.SegmentDuration = 24 * time.Hour
	base := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(24 * time.Hour)
	var peak int64
	for day := 0; day < 30; day++ {
		for slot := 0; slot < 72; slot++ {
			at := base.Add(time.Duration(day)*24*time.Hour + time.Duration(slot)*20*time.Minute)
			for node := 0; node < 6; node++ {
				b := TelemetryBatch{Source: fmt.Sprint(node), Identity: fmt.Sprintf("%d/%d", day, slot)}
				for key := 0; key < 29; key++ {
					b.Points = append(b.Points, Point{MetricName: fmt.Sprintf("chart.%d", key), EntityID: fmt.Sprint(node), Timestamp: at, Value: float64(day*72 + slot + key)})
				}
				if _, err := e.Accept(ctx, b, at); err != nil {
					t.Fatal(day, slot, err)
				}
			}
			lifecycleArchiveAll(t, e, at.Add(time.Minute))
			promoteTelemetryAll(t, e, at)
			auditDrainCatalog(t, e, at)
			if n := telemetryCount(t, e, "segments"); n > int64(6*(day+1)) {
				t.Fatal("owners grow by handoff", day, slot, n)
			}
			if n := telemetryCount(t, e, "members"); n > int64(6*29*(day+1)) {
				t.Fatal("members grow by handoff", day, slot, n)
			}
			var disk int64
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if st, err := os.Stat(dir + "/metrics.db" + suffix); err == nil {
					disk += st.Size()
				}
			}
			peak = max(peak, disk)
		}
	}
	if n := telemetryCount(t, e, "segments"); n != 180 {
		t.Fatal(n)
	}
	if n := telemetryCount(t, e, "members"); n != 5220 {
		t.Fatal(n)
	}
	if peak > 40<<20 {
		t.Fatal("growing SQL exceeds monitoring budget", peak)
	}
	t.Logf("handoffs=12960 owners=180 members=5220 growing_sql_wal_shm_peak=%d (no VACUUM); per-table pages measured independently by Python SQLite", peak)
}

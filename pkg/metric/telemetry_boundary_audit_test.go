package metric

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestTelemetryArchiveExpiryUsesLastObservation(t *testing.T) {
	for _, imported := range []bool{false, true} {
		t.Run(fmt.Sprint("imported=", imported), func(t *testing.T) {
			s, e := telemetryFixture(t, "")
			ctx := WithTelemetryEntities(context.Background(), []string{"node"})
			day := time.Now().UTC().Add(-31 * 24 * time.Hour).Truncate(24 * time.Hour)
			first, last := day.Add(2*time.Second), day.Add(24*time.Hour-time.Second)
			e.cfg.SegmentDuration = 24 * time.Hour
			e.cfg.MaxLateness = 365 * 24 * time.Hour
			clock := first
			if imported {
				clock = day.Add(31 * 24 * time.Hour)
			}
			for i, at := range []time.Time{first, last} {
				if !imported {
					clock = at
				}
				b := telemetryBatch(fmt.Sprint(i), at, float64(i+1))
				// A different series supplies the actual newest observation.
				if i == 1 {
					b.Points[0].MetricName = "memory"
				}
				if _, err := e.Accept(ctx, b, clock); err != nil {
					t.Fatal(err)
				}
			}
			lifecycleArchiveAll(t, e, clock.Add(time.Minute))
			if n := telemetryCount(t, e, "segments"); n != 1 {
				t.Fatal("fixture must be one full-day segment", n)
			}
			retention := 30 * 24 * time.Hour
			if ok, err := e.ExpireArchiveNext(ctx, first.Add(retention+time.Second), retention); err != nil || ok {
				t.Fatal("deleted a segment with a retained tail", ok, err)
			}
			q := []Query{{MetricName: "memory", EntityID: "node", Start: last, End: last.Add(time.Second)}}
			if points, err := e.ExportRawBatch(ctx, q); err != nil || len(points[0]) != 1 || points[0][0].Value != 2 {
				t.Fatal("retained tail unavailable", points, err)
			}
			if ok, err := e.ExpireArchiveNext(ctx, last.Add(retention), retention); err != nil || ok {
				t.Fatal("inclusive last observation expired at cutoff", ok, err)
			}
			// Imported owners were created after this expiry time. Observation
			// age, rather than that wall clock, must still reclaim them now.
			if ok, err := e.ExpireArchiveNext(ctx, last.Add(retention+time.Nanosecond), retention); err != nil || !ok {
				t.Fatal("fully aged segment was kept", ok, err)
			}
			var state string
			if err := s.db.QueryRow("SELECT state FROM " + e.table("segments")).Scan(&state); err != nil || state != "expired" {
				t.Fatal(state, err)
			}
		})
	}
}

func TestTelemetryArchiveExpiryPreservesSharedFileAndResumes(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := WithTelemetryEntities(context.Background(), []string{"node"})
	day := time.Now().UTC().Add(-31 * 24 * time.Hour).Truncate(24 * time.Hour)
	for i, at := range []time.Time{day.Add(time.Hour), day.Add(23 * time.Hour)} {
		if _, err := e.Accept(ctx, telemetryBatch(fmt.Sprint(i), at, float64(i+1)), at); err != nil {
			t.Fatal(err)
		}
		lifecycleArchiveAll(t, e, at.Add(time.Minute))
	}
	var id string
	if err := s.db.QueryRow("SELECT id FROM " + e.table("segments") + " ORDER BY archive_offset LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	seg, err := e.segment(ctx, s.db, id)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("expiry interrupted")
	e.cfg.Fault = func(stage string) error {
		if stage == "expire-after-remove" {
			return failure
		}
		return nil
	}
	now := day.Add(30*24*time.Hour + 12*time.Hour)
	if _, err = e.ExpireArchiveNext(ctx, now, 30*24*time.Hour); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	e.cfg.Fault = nil
	if ok, err := e.ExpireArchiveNext(ctx, now, 30*24*time.Hour); err != nil || !ok {
		t.Fatal("expiring did not resume", ok, err)
	}
	if f, err := e.root.Open(e.segmentPath(seg)); err != nil {
		t.Fatal("shared file removed with retained owner", err)
	} else {
		f.Close()
	}
	q := []Query{{MetricName: "cpu", EntityID: "node", Start: day.Add(23 * time.Hour), End: day.Add(24 * time.Hour)}}
	if points, err := e.ExportRawBatch(ctx, q); err != nil || len(points[0]) != 1 || points[0][0].Value != 2 {
		t.Fatal("retained shared extent", points, err)
	}
	// A genuinely empty owner still needs an age fallback after erasure.
	if _, err = s.db.Exec("DELETE FROM " + e.table("members")); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.ExpireArchiveNext(ctx, day.Add(32*24*time.Hour), 30*24*time.Hour); err != nil || !ok {
		t.Fatal("empty owner was stranded", ok, err)
	}
	if f, err := e.root.Open(e.segmentPath(seg)); !os.IsNotExist(err) {
		if f != nil {
			f.Close()
		}
		t.Fatal("last owner did not remove file", err)
	}
}

func TestTelemetryChartPlansAcrossPrunedSourceBucket(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(10 * time.Second).Add(5 * time.Second)
	cutoff := now.Add(-e.cfg.RawWindow)
	for _, name := range []string{"cpu", sqliteMergedPingLatencyMetric} {
		for i, offset := range []time.Duration{-3 * time.Second, 3 * time.Second, 7 * time.Second, 13 * time.Second} {
			at := cutoff.Add(offset)
			b := telemetryBatch(fmt.Sprintf("%s/%d", name, i), at, float64(i+1))
			b.Points[0].MetricName = name
			if _, err := e.Accept(ctx, b, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	lifecycleArchiveAll(t, e, now)
	var pruned int
	if err := e.s.db.QueryRow("SELECT COUNT(*) FROM "+e.table("hot")+" WHERE observed<?", cutoff.UnixNano()).Scan(&pruned); err != nil || pruned != 0 {
		t.Fatal("fixture must actually prune the old population", pruned, err)
	}
	for _, name := range []string{"cpu", sqliteMergedPingLatencyMetric} {
		unplanned := AggregateQuery{Query: Query{MetricName: name, EntityID: "node", Start: cutoff.Add(time.Second), End: now}, Interval: 10 * time.Second, Aggregation: AggSum, PreserveSeries: true}
		if r, err := e.Chart(ctx, unplanned, now); err != nil || len(r.Gaps) != 1 {
			t.Fatal("fixture must expose the incomplete source bucket", r, err)
		}
		for _, tc := range []struct {
			name            string
			offset, advance time.Duration
			count           int
		}{
			{"before_cutoff", -time.Second, 6 * time.Second, 2},
			{"at_cutoff", 0, 5 * time.Second, 2},
			{"inside_straddling_bucket", time.Second, 4 * time.Second, 2},
			{"complete_bucket", 5 * time.Second, 0, 2},
			{"exact_hot_refinement", 6 * time.Second, 0, 2},
			{"ordinary_14m30s", 30 * time.Second, 0, 0},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				start := cutoff.Add(tc.offset)
				q, step, err := e.PlanChart(ctx, Query{MetricName: name, EntityID: "node", Start: start, End: now}, now, 600, 30)
				if err != nil || step != 10*time.Second || !q.Start.Equal(start.Add(tc.advance)) || !q.End.Equal(now) {
					t.Fatal("source bucket boundary planning", q, step, err)
				}
				r, err := e.Chart(ctx, AggregateQuery{Query: q, Interval: step, Aggregation: AggSum, PreserveSeries: true}, now)
				if err != nil || len(r.Gaps) != 0 {
					t.Fatal("planned request has a gap", r, err)
				}
				count := 0
				for _, p := range r.Points {
					count += p.Count
				}
				if count != tc.count {
					t.Fatal("lost or invented observations", count, tc.count)
				}
			})
		}
	}
	// The right edge has the same source-completeness requirement.
	_, end, err := e.ChartWindow(cutoff.Add(-time.Minute), cutoff.Add(time.Second), 10*time.Second, now)
	if err != nil || !end.Equal(alignTime(cutoff, 10*time.Second).Add(-time.Nanosecond)) {
		t.Fatal("historical right source boundary", end, err)
	}
}

func TestTelemetryMigrationPressureSelectsOnePartition(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	e.cfg.SegmentBytes = 8 << 10
	e.cfg.MaxLateness = 365 * 24 * time.Hour
	for node := 0; node < 6; node++ {
		for day := 0; day < 30; day++ {
			b := telemetryBatch(fmt.Sprintf("%d/%d", node, day), now.Add(-time.Duration(day)*24*time.Hour), 1)
			b.Source = fmt.Sprint(node)
			b.Points[0].EntityID = b.Source
			if node == 0 && day == 0 {
				for i := 1; i < 10; i++ {
					p := b.Points[0]
					p.Value = float64(i)
					b.Points = append(b.Points, p)
				}
			}
			if _, err := e.Accept(ctx, b, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	hot, err := e.progress(ctx, e.s.db, "hot_bytes")
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.MaxHotBytes = hot * 2
	n, err := e.sealDue(ctx, now.Add(25*time.Hour), false)
	if err != nil || n != 1 {
		t.Fatal("pressure flushed small tails", n, err)
	}
	var open, sealedPoints int
	if err = e.s.db.QueryRow("SELECT COUNT(*) FROM " + e.table("segments") + " WHERE state='open'").Scan(&open); err != nil || open != 179 {
		t.Fatal(open, err)
	}
	if err = e.s.db.QueryRow("SELECT points FROM " + e.table("segments") + " WHERE state='sealed'").Scan(&sealedPoints); err != nil || sealedPoints != 10 {
		t.Fatal("did not choose largest", sealedPoints, err)
	}
	// Per-partition limits still apply independently of the pressure selector.
	e.cfg.SegmentPoints = 1
	n, err = e.sealDue(ctx, now, false)
	if err != nil || n != 179 {
		t.Fatal("ignored per-partition limit", n, err)
	}
}

func TestTelemetryWildcardRawBudgetCannotSilentlyDropEntities(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := WithTelemetryEntities(context.Background(), []string{"node", "second"})
	now := time.Now().UTC()
	at := now.Add(-2 * time.Minute)
	for _, entity := range []string{"node", "second"} {
		b := TelemetryBatch{Source: entity, Identity: "budget"}
		for i := 0; i < 5001; i++ {
			b.Points = append(b.Points, Point{EntityID: entity, MetricName: "cpu", Timestamp: at.Add(time.Duration(i) * time.Millisecond), Value: float64(i)})
		}
		for offset := 0; offset < len(b.Points); offset += 256 {
			batch := TelemetryBatch{Source: entity, Identity: fmt.Sprint("budget/", offset), Points: b.Points[offset:min(offset+256, len(b.Points))]}
			if _, err := e.Accept(ctx, batch, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	q := Query{MetricName: "cpu", Start: at, End: now}
	if points, err := e.ReadHot(ctx, q, now); !errors.Is(err, ErrTelemetryBackpressure) || points != nil {
		t.Fatal("default multi-entity read silently truncated", len(points), err)
	}
	q.Limit = 100
	if points, err := e.ReadHot(ctx, q, now); err != nil || len(points) != 100 {
		t.Fatal("explicit paging changed", len(points), err)
	}
	q.Limit = 0
	q.EntityID = "second"
	if points, err := e.ReadHot(ctx, q, now); err != nil || len(points) != 5001 {
		t.Fatal("bounded single entity read", len(points), err)
	}
}

func TestTelemetryMigrationCatalogFinishesBeforeCompletionAndResumes(t *testing.T) {
	ctx := context.Background()
	s, e := telemetryFixture(t, "")
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-24 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour)
	if err := s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	var source []Point
	for i := 0; i < 12; i++ {
		source = append(source, Point{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(time.Duration(i) * time.Hour), Value: float64(i + 1)})
	}
	if err := s.WriteBatch(ctx, source); err != nil {
		t.Fatal(err)
	}
	cfg := e.cfg
	cfg.SegmentPoints = 3
	failure := errors.New("catalog completion interrupted")
	cfg.Fault = func(stage string) error {
		if stage == "catalog-merge-before-commit" {
			return failure
		}
		return nil
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateLegacy(ctx, cfg, nil); !errors.Is(err, failure) {
		t.Fatal("catalog skipped before completion", err)
	}
	var complete, checkpoints, receipts int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + e.table("progress") + " WHERE task='legacy_migration_complete' AND value=1").Scan(&complete); err != nil || complete != 0 {
		t.Fatal("published premature completion", complete, err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + e.table("progress") + " WHERE task LIKE 'm/%'").Scan(&checkpoints); err != nil || checkpoints == 0 {
		t.Fatal("lost retry checkpoints", checkpoints, err)
	}
	if receipts = int(telemetryCount(t, e, "receipts")); receipts != 0 {
		t.Fatal("finalized import receipts not retired", receipts)
	}
	cfg.Fault = nil
	for i := 0; i < 2; i++ {
		if err := s.MigrateLegacy(ctx, cfg, nil); err != nil {
			t.Fatal("resume", err)
		}
	}
	if err := s.EnableTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	e = s.Telemetry()
	if n := telemetryCount(t, e, "segments"); n != 1 {
		t.Fatal("completed migration left fragmented owners", n)
	}
	if n := telemetryCount(t, e, "members"); n != 1 {
		t.Fatal("completed migration left repeated members", n)
	}
	q := Query{MetricName: "cpu", EntityID: "node", Start: at, End: now}
	snapshotDir := t.TempDir()
	snapshot, err := e.FreezeExport(WithTelemetryEntities(ctx, []string{"node"}), []Query{q}, snapshotDir, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	rawCount, rawSum := 0, 0.0
	for i := range snapshot.Units {
		if err = VisitExportInput(ctx, snapshotDir, snapshot, i, func(p ExportObservation) error { rawCount++; rawSum += p.Point.Value; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if rawCount != len(source) || rawSum != 78 {
		t.Fatal("resume changed raw population", rawCount, rawSum)
	}
	r, err := e.Chart(ctx, AggregateQuery{Query: q, Interval: time.Hour, Aggregation: AggSum, PreserveSeries: true}, now)
	if err != nil || len(r.Gaps) != 0 {
		t.Fatal(r, err)
	}
	count, sum := 0, 0.0
	for _, p := range r.Points {
		count += p.Count
		sum += p.Value
	}
	if count != 12 || sum != 78 {
		t.Fatal("resume duplicated population", count, sum)
	}
}

package metric

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func peripheralStats(t *testing.T, e *Telemetry) map[string]string {
	t.Helper()
	rows, err := e.s.db.Query("SELECT s.metric,s.entity,s.tags,SUM(r.n),SUM(r.loss),SUM(r.total) FROM " + e.table("rollup") + " r JOIN " + e.table("rollup_keys") + " k ON k.id=r.series_id JOIN " + e.table("series") + " s ON s.id=k.source_id GROUP BY s.metric,s.entity,s.tags")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var metric, entity, tags string
		var n, loss int64
		var sum float64
		if err := rows.Scan(&metric, &entity, &tags, &n, &loss, &sum); err != nil {
			t.Fatal(err)
		}
		result[metric+"/"+entity+"/"+tags] = fmt.Sprintf("%d/%d/%g", n, loss, sum)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTelemetryExportClosedEndHotAndCold(t *testing.T) {
	for _, cold := range []bool{false, true} {
		t.Run(fmt.Sprint(cold), func(t *testing.T) {
			_, e := telemetryFixture(t, "")
			ctx := WithTelemetryEntities(context.Background(), []string{"node"})
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			points := []Point{}
			for i, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
				points = append(points, Point{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(delta), Value: float64(i)})
			}
			if _, err := e.Accept(ctx, TelemetryBatch{Source: "node", Identity: "end", Points: points}, at); err != nil {
				t.Fatal(err)
			}
			if cold {
				lifecycleArchiveAll(t, e, time.Now())
			}
			q := []Query{{MetricName: "cpu", EntityID: "node", Start: at.Add(-time.Second), End: at}}
			want, err := e.ExportRawBatch(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			snap, err := e.FreezeExport(ctx, q, dir, 8<<20)
			if err != nil {
				t.Fatal(err)
			}
			got := exportSnapshotPoints(t, dir, snap)
			if len(want[0]) != 2 || len(got) != 2 || !got[1].Point.Timestamp.Equal(at) {
				t.Fatal("inconsistent closed end", want, got)
			}
		})
	}
}

func TestTelemetryScalarWindowUsesActualRawPopulation(t *testing.T) {
	for _, mode := range []string{"live", "missing-hot", "legacy-60s"} {
		t.Run(mode, func(t *testing.T) {
			s, e := telemetryFixture(t, "")
			ctx := WithTelemetryEntities(context.Background(), []string{"visible"})
			s.telemetry.Store(e)
			t.Cleanup(func() { s.telemetry.Store(nil) })
			now := time.Now().UTC().Truncate(time.Minute).Add(55 * time.Second)
			step := 10 * time.Second
			if mode == "legacy-60s" {
				step = time.Minute
			}
			at := now.Add(-3 * time.Minute).Truncate(step)
			if err := s.CreateMetric(ctx, Definition{Name: sqliteMergedPingLatencyMetric, RetentionDays: 30}); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"visible", "hidden"} {
				if mode == "legacy-60s" {
					var cells []telemetryMigrationCell
					for i := 0; i < 3; i++ {
						var st telemetryStats
						bucket := at.Add(time.Duration(i) * step)
						for j, v := range []float64{10, -1, 30} {
							addScalarStats(&st, Point{MetricName: sqliteMergedPingLatencyMetric, Timestamp: bucket.Add(time.Duration(j*10) * time.Second), Value: v}, 1, j)
						}
						cells = append(cells, telemetryMigrationCell{series: telemetryMigrationKey(id), tags: "{\"task_id\":\"1\"}", bucket: bucket.UnixNano(), resolution: step, stats: st})
					}
					if err := e.commitScalarLegacyCells(ctx, "import/"+id, sqliteMergedPingLatencyMetric, id, cells); err != nil {
						t.Fatal(err)
					}
				} else {
					var points []Point
					for i := 0; i < 3; i++ {
						for j, v := range []float64{10, -1, 30} {
							points = append(points, Point{MetricName: sqliteMergedPingLatencyMetric, EntityID: id, Timestamp: at.Add(time.Duration(i)*step + time.Duration(j)*time.Second), Value: v, Tags: map[string]string{"task_id": "1"}})
						}
					}
					if _, err := e.Accept(ctx, TelemetryBatch{Source: id, Identity: "stats", Points: points}, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode != "legacy-60s" {
				summarizeAll(t, e)
			}
			if mode == "missing-hot" {
				if _, err := s.db.Exec("DELETE FROM " + e.table("hot")); err != nil {
					t.Fatal(err)
				}
			}
			// Cross first and last populations; only the middle cell is complete
			// without raw. Hidden entities may never affect the advertised window.
			q := Query{MetricName: sqliteMergedPingLatencyMetric, Start: at.Add(time.Second), End: at.Add(2*step + time.Second)}
			planned, interval, err := e.PlanChart(ctx, q, now, 600, 30)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "live" {
				if !planned.Start.Equal(q.Start) || !planned.End.Equal(q.End) {
					t.Fatal("trimmed complete live raw", planned)
				}
			} else if !planned.Start.Equal(at.Add(step)) || !planned.End.Equal(at.Add(2*step-time.Nanosecond)) {
				t.Fatal("did not advertise inward window", planned, step)
			}
			values, err := s.PingSummariesForEntities(ctx, AggregateQuery{Query: planned, Interval: interval}, []string{"visible"}, now)
			if err != nil {
				t.Fatal(err)
			}
			summary := values["visible"]
			if len(summary.Loss) != 1 || summary.Loss[0].EntityID != "visible" {
				t.Fatal("scope or missing card", summary)
			}
			want := 3
			if mode == "live" {
				want = 7
			}
			if summary.Loss[0].Count != want {
				t.Fatal("partial denominator", summary.Loss, want)
			}
		})
	}
}

func TestTelemetryScalarSummarySchedulesRealDeadline(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	if err := e.EnsureSchedule(ctx, []ScheduleTask{{Name: "tiers", Due: now.Add(24 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := e.Accept(ctx, telemetryBatch(fmt.Sprint(i), now.Add(time.Duration(i)*time.Second), 3), now); err != nil {
			t.Fatal(err)
		}
		summarizeAll(t, e)
		var ns int64
		if err := s.db.QueryRow("SELECT due FROM " + e.table("schedule") + " WHERE task='tiers'").Scan(&ns); err != nil {
			t.Fatal(err)
		}
		want := alignTime(now, 10*time.Second).Add(10*time.Second + telemetrySummaryKeep(10*time.Second))
		if !time.Unix(0, ns).Equal(want) {
			t.Fatal("tiers woken immediately or delayed", time.Unix(0, ns), want)
		}
	}
	// A live lease must not be mistaken for an expired lease just because the
	// requested promotion deadline lies in the future.
	lease := now.Add(time.Minute).UnixNano()
	if _, err := s.db.Exec("UPDATE "+e.table("schedule")+" SET due=?,expires=?", now.Add(24*time.Hour).UnixNano(), lease); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Accept(ctx, telemetryBatch("leased", now, 4), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	var ns int64
	if err := s.db.QueryRow("SELECT due FROM " + e.table("schedule")).Scan(&ns); err != nil || ns != now.Add(24*time.Hour).UnixNano() {
		t.Fatal("modified active lease", ns, err)
	}
}

func TestTelemetryScalarErasedSweepDoesNotAcquireWriter(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	// Hold the writer gate; scalar pruning has no old summary population.
	release, err := e.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if n, err := e.PruneErasedChunk(short, 128); err != nil || n != 0 {
		t.Fatal("scalar empty sweep acquired writer", n, err)
	}
}

func TestTelemetryRollbackReupgradeDropsEveryTelemetryObject(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	for _, name := range []string{"cpu", sqliteMergedPingLatencyMetric} {
		if err := s.CreateMetric(ctx, Definition{Name: name, RetentionDays: 30}); err != nil {
			t.Fatal(err)
		}
	}
	for _, entity := range []string{"z-node", "a-node"} {
		for _, task := range []string{"2", "1"} {
			points := []Point{{MetricName: "cpu", EntityID: entity, Timestamp: at, Value: 3, Tags: map[string]string{"task_id": task}}, {MetricName: "cpu", EntityID: entity, Timestamp: at.Add(time.Second), Value: 7, Tags: map[string]string{"task_id": task}}, {MetricName: sqliteMergedPingLatencyMetric, EntityID: entity, Timestamp: at, Value: 21, Tags: map[string]string{"task_id": task}}, {MetricName: sqliteMergedPingLatencyMetric, EntityID: entity, Timestamp: at.Add(time.Second), Value: -1, Tags: map[string]string{"task_id": task}}}
			if _, err := e.Accept(ctx, TelemetryBatch{Source: entity, Identity: entity + task, Points: points}, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.finishLegacyRawHandoff(ctx); err != nil {
		t.Fatal(err)
	}
	want := peripheralStats(t, e)
	if len(want) != 8 {
		t.Fatal(want)
	}
	output := filepath.Join(t.TempDir(), "rollback.db")
	if _, err := e.BeginLegacyPointImport(ctx, "unused-source", "records"); err != nil {
		t.Fatal(err)
	}
	if err := e.RollbackLegacy(ctx, output, nil); err != nil {
		t.Fatal(err)
	}
	check, err := sql.Open("sqlite3", output)
	if err != nil {
		t.Fatal(err)
	}
	var objects int
	err = check.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'metric_telemetry_%'").Scan(&objects)
	check.Close()
	if err != nil || objects != 0 {
		t.Fatalf("rollback retained %d telemetry objects: %v", objects, err)
	}
	old, err := Open(ctx, SQLite(output, WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	cfg := TelemetryConfig{ArchiveDirectory: filepath.Join(t.TempDir(), "new-archive")}
	for i := 0; i < 2; i++ {
		if err = old.MigrateLegacy(ctx, cfg, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err = old.EnableTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := peripheralStats(t, old.Telemetry()); !reflect.DeepEqual(want, got) {
		t.Fatalf("reupgrade altered counts/sums/loss or series: want=%v got=%v", want, got)
	}
}

func TestTelemetryProductionUpstream131OpenMigrateReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dsn := sqliteFileDSN(filepath.Join(dir, "metrics.db"))
	at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	seedUpstream131SQLiteStore(t, dsn, at, false)
	cfg := TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")}
	for i := 0; i < 2; i++ {
		s, err := Open(ctx, SQLite(dsn, WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err = s.RequireTelemetryMigration(ctx); err == nil {
				t.Fatal("unconverted history was hidden")
			}
		}
		if err = s.MigrateLegacy(ctx, cfg, nil); err != nil {
			t.Fatal(err)
		}
		if err = s.EnableTelemetry(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		q := AggregateQuery{Query: Query{MetricName: "compat.131", EntityID: "upstream-node", Start: at, End: at.Add(time.Minute - time.Nanosecond), Tags: map[string]string{"task_id": "7"}}, Interval: time.Minute, Aggregation: AggSum}
		r, err := s.Telemetry().Chart(ctx, q, time.Now().UTC())
		if err != nil || len(r.Points) != 1 || r.Points[0].Count != 2 || r.Points[0].Value != 30 {
			t.Fatal(r, err)
		}
		for _, table := range []string{s.tables.pointBlocks, s.tables.rollupBlocks} {
			var n int
			if err = s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
				t.Fatal("adapter wrote compressed SQL blocks", n, err)
			}
		}
		s.Close()
	}
}

func TestTelemetryLegacyPointImportAtomicResumeAndAge(t *testing.T) {
	for _, stage := range []string{"ingest-before-commit", "ingest-after-commit"} {
		t.Run(stage, func(t *testing.T) {
			s, e := telemetryFixture(t, "")
			ctx := context.Background()
			for _, d := range []Definition{{Name: "cpu", RetentionDays: 30}, {Name: sqliteMergedPingLatencyMetric, RetentionDays: 30}, {Name: "disabled", RetentionDays: 0}} {
				if err := s.CreateMetric(ctx, d); err != nil {
					t.Fatal(err)
				}
			}
			at := time.Now().UTC().Add(-6 * 24 * time.Hour).Truncate(24 * time.Hour)
			var points []Point
			for _, entity := range []string{"node-a", "node-b"} {
				for d := 0; d < 2; d++ {
					for i := 0; i < 800; i++ {
						name, value := "cpu", float64(i%10)
						if i%10 == 0 {
							name, value = sqliteMergedPingLatencyMetric, -1
						}
						if i%10 == 1 {
							name = "disabled"
						}
						points = append(points, Point{MetricName: name, EntityID: entity, Timestamp: at.Add(time.Duration(d)*24*time.Hour + time.Duration(i)*time.Second), Value: value})
					}
				}
			}
			writer, err := e.BeginLegacyPointImport(ctx, "production-copy", "records")
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("injected interruption")
			fired := false
			e.cfg.Fault = func(s string) error {
				if s == stage && !fired {
					fired = true
					return failure
				}
				return nil
			}
			if err = writer.Write(ctx, points); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			var cursor int64
			if err = s.db.QueryRow("SELECT position FROM " + e.table("imports")).Scan(&cursor); err != nil {
				t.Fatal(err)
			}
			if (stage == "ingest-before-commit" && cursor != 0) || (stage == "ingest-after-commit" && cursor != 256) {
				t.Fatalf("cursor not atomic: %d", cursor)
			}
			e.cfg.Fault = nil
			writer, err = e.BeginLegacyPointImport(ctx, "production-copy", "records")
			if err != nil {
				t.Fatal(err)
			}
			if err = writer.Write(ctx, points); err != nil {
				t.Fatal(err)
			}
			if err = writer.ValidateComplete(); err != nil {
				t.Fatal(err)
			}
			if err = e.FinishLegacyPointImport(ctx); err != nil {
				t.Fatal(err)
			}
			want := peripheralStats(t, e)
			if len(want) != 4 {
				t.Fatal(want)
			}
			for key, value := range want {
				if value != "1280/0/7040" && value != "0/160/0" {
					t.Fatal(key, value)
				}
			}
			for _, table := range []string{s.tables.pointValues, s.tables.pointBlocks, s.tables.rollupValues, s.tables.rollupBlocks} {
				var n int
				if err = s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
					t.Fatal("legacy staging grew", table, n, err)
				}
			}
			if n := telemetryCount(t, e, "hot"); n != 0 {
				t.Fatal("old raw SQL retained", n)
			}
			if n := telemetryCount(t, e, "segments"); n != 4 {
				t.Fatal("machine/day catalog not converged", n)
			}
			var finest int64
			if err = s.db.QueryRow("SELECT MIN(resolution) FROM " + e.table("rollup")).Scan(&finest); err != nil || finest < 1800 {
				t.Fatal("old data retained fine tier", finest, err)
			}
			for {
				n, err := e.PruneReceipts(ctx, time.Now().Add(7*24*time.Hour), 512)
				if err != nil {
					t.Fatal(err)
				}
				if n == 0 {
					break
				}
			}
			if n := telemetryCount(t, e, "receipts"); n != 0 {
				t.Fatal("receipt retirement fixture incomplete", n)
			}
			writer, err = e.BeginLegacyPointImport(ctx, "production-copy", "records")
			if err != nil {
				t.Fatal(err)
			}
			if err = writer.Write(ctx, points); err != nil {
				t.Fatal(err)
			}
			if err = e.FinishLegacyPointImport(ctx); err != nil {
				t.Fatal(err)
			}
			if got := peripheralStats(t, e); !reflect.DeepEqual(want, got) {
				t.Fatal("retry after receipt GC counted twice", want, got)
			}
			short, err := e.BeginLegacyPointImport(ctx, "production-copy", "records")
			if err != nil {
				t.Fatal(err)
			}
			if err = short.Write(ctx, points[:100]); err != nil {
				t.Fatal(err)
			}
			if err = short.ValidateComplete(); err == nil {
				t.Fatal("shrunk source accepted")
			}
		})
	}
}

package metric

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Fixtures that explicitly model the old 10s/5m/15m/1h online layout must
// exercise that producer, not fabricate tiers absent from the new layout.
func useLegacyOnlineTiers(t *testing.T, e *Telemetry) {
	t.Helper()
	if _, err := e.s.db.Exec("UPDATE " + e.table("segments") + " SET summary_version=1 WHERE state<>'legacy'"); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryTiersManySeriesAvoidReportTimeWriteAmplification(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-6 * time.Hour)
	for part := 0; part < 4; part++ {
		b := TelemetryBatch{Source: "node", Identity: fmt.Sprintf("cardinality/%d", part)}
		for series := part * 32; series < (part+1)*32; series++ {
			for i := 0; i < 6; i++ {
				b.Points = append(b.Points, Point{MetricName: "cpu", EntityID: "node", Tags: map[string]string{"device": fmt.Sprint(series)}, Timestamp: at.Add(time.Duration(i) * 10 * time.Second), Value: float64(series)})
			}
		}
		if _, err := e.Accept(ctx, b, now); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	summarizeAll(t, e)
	baseTime := time.Since(started)
	if n := telemetryCount(t, e, "summary"); n != 768 {
		t.Fatal("report fanout wrote redundant tiers", n)
	}
	if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	promoteTelemetryAll(t, e, now)
	t.Logf("128 series / 768 points: finest summary %s, one-time promotion %s", baseTime, time.Since(started))
	if n := telemetryCount(t, e, "summary"); n != 1152 {
		t.Fatal("unbounded duplicate parent cells", n)
	}
	r, err := e.Chart(ctx, AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute - time.Nanosecond)}, Interval: time.Minute, Aggregation: AggAvg, PreserveSeries: true}, now)
	if err != nil || len(r.Points) != 128 || len(r.Gaps) > 0 {
		t.Fatal("cardinality population", len(r.Points), err)
	}
	for _, p := range r.Points {
		if p.Count != 6 {
			t.Fatal(p)
		}
	}
}

func TestTelemetryTiersAllChartWindowsMatchRawAfterRetention(t *testing.T) {
	for _, hours := range []int{1, 6, 12, 24, 168, 360, 720, 1080, 1440, 1800, 2160, 2520} {
		t.Run(fmt.Sprint(hours), func(t *testing.T) {
			_, e := telemetryFixture(t, "")
			ctx := context.Background()
			now := time.Now().UTC().Truncate(24 * time.Hour)
			span := time.Duration(hours) * time.Hour
			e.cfg.MaxLateness = 120 * 24 * time.Hour
			points := 360
			if hours == 1800 {
				points = 300
			}
			if hours == 2160 {
				points = 309
			}
			interval := TelemetryChartInterval(span, points)
			start := alignTime(now.Add(-span), interval)
			b := TelemetryBatch{Source: "node", Identity: "windows"}
			for at := start; at.Before(now); at = at.Add(interval) {
				b.Points = append(b.Points, Point{MetricName: "cpu", EntityID: "node", Timestamp: at, Value: float64(len(b.Points) + 1)})
			}
			for offset := 0; offset < len(b.Points); offset += 180 {
				part := TelemetryBatch{Source: b.Source, Identity: fmt.Sprintf("windows/%d", offset), Points: b.Points[offset:min(offset+180, len(b.Points))]}
				if _, err := e.Accept(ctx, part, now); err != nil {
					t.Fatal(err)
				}
			}
			summarizeAll(t, e)
			if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			promoteTelemetryAll(t, e, now)
			for {
				n, err := e.PruneSummaries(ctx, "cpu", 120*24*time.Hour, now, 512)
				if err != nil {
					t.Fatal(err)
				}
				if n == 0 {
					break
				}
			}
			q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: start, End: now.Add(-time.Nanosecond)}, Interval: interval, Aggregation: AggAvg, PreserveSeries: true}
			want, err := AggregatePoints(b.Points, q)
			if err != nil {
				t.Fatal(err)
			}
			r, err := e.Chart(ctx, q, now)
			if err != nil || len(r.Points) != len(want) || len(r.Gaps) > 0 || len(r.Points) < 300 || len(r.Points) > 361 {
				t.Fatalf("%dh: %d/%d points gaps %d err %v", hours, len(r.Points), len(want), len(r.Gaps), err)
			}
			for i, p := range r.Points {
				if p.Value != want[i].Value || p.Count != want[i].Count || !p.Bucket.Equal(want[i].Bucket) {
					t.Fatal("changed raw population", p, want[i])
				}
			}
		})
	}
}

func TestTelemetryTiersOpenSegmentCannotPromoteAndSchemaUpgradePreservesOldLayout(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	if _, err := e.Accept(ctx, telemetryBatch("open", now.Add(-time.Hour), 3), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if n, err := e.PromoteSummariesChunk(ctx, now, 512); err != nil || n != 0 {
		t.Fatal("open segment promoted", n, err)
	}
	useLegacyOnlineTiers(t, e)
	for _, query := range []string{
		"UPDATE " + e.table("progress") + " SET value=4 WHERE task='schema_version'",
		"DROP INDEX " + s.cfg.TablePrefix + "telemetry_summary_pending",
		"ALTER TABLE " + e.table("segments") + " DROP COLUMN summary_version",
		"ALTER TABLE " + e.table("summary") + " DROP COLUMN input_through",
		"ALTER TABLE " + e.table("summary") + " DROP COLUMN promoted_through",
	} {
		if _, err := s.db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := s.OpenTelemetry(ctx, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version, layout int
	if err := s.db.QueryRow("SELECT value FROM " + e.table("progress") + " WHERE task='schema_version'").Scan(&version); err != nil || version != 7 {
		t.Fatal(version, err)
	}
	if err := s.db.QueryRow("SELECT summary_version FROM " + e.table("segments")).Scan(&layout); err != nil || layout != 1 {
		t.Fatal("old segments reinterpreted", layout, err)
	}
	if _, err := upgraded.Accept(ctx, telemetryBatch("old-compatible", now, 7), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, upgraded)
	var coarse int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM "+e.table("summary")+" WHERE resolution=?", int64(time.Hour)).Scan(&coarse); err != nil || coarse != 1 {
		t.Fatal("old writer compatibility", coarse, err)
	}
}

func TestTelemetryTiersSealingCursorAtomicityAndPartialPromotion(t *testing.T) {
	ctx := context.Background()
	_, e := telemetryFixture(t, "")
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-6 * time.Hour)
	b := TelemetryBatch{Source: "node", Identity: "promotion"}
	for i := 0; i < 180; i++ {
		b.Points = append(b.Points, Point{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(time.Duration(i) * 10 * time.Second), Value: float64(i)})
	}
	if _, err := e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	if n, err := e.PromoteSummariesChunk(ctx, now, 1); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := e.PromoteSummariesChunk(ctx, now, 1); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	summarizeAll(t, e)
	// GC cannot drop one fine cell before its exact immutable input handoff.
	if n, err := e.PruneSummaries(ctx, "cpu", 30*24*time.Hour, now, 512); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	for _, stage := range []string{"tier-parent-before-handoff", "tier-before-commit"} {
		e.cfg.Fault = func(s string) error {
			if s == stage {
				return errors.New("power loss")
			}
			return nil
		}
		if _, err := e.PromoteSummariesChunk(ctx, now, 1); err == nil {
			t.Fatal("missing injected failure", stage)
		}
		if n := telemetryCount(t, e, "summary"); n != 180 {
			t.Fatal("partial parent published", n)
		}
	}
	e.cfg.Fault = nil
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(30*time.Minute - time.Nanosecond)}, Interval: 15 * time.Minute, Aggregation: AggSum, PreserveSeries: true}
	want, err := AggregatePoints(b.Points, q)
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 50; pass++ {
		n, err := e.PromoteSummariesChunk(ctx, now, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.PruneSummaries(ctx, "cpu", 30*24*time.Hour, now, 512); err != nil {
			t.Fatal(err)
		}
		r, err := e.Chart(ctx, q, now)
		if err != nil || len(r.Gaps) > 0 || len(r.Points) != len(want) {
			t.Fatalf("partial pass %d: %+v %v", pass, r, err)
		}
		for i, p := range r.Points {
			if p.Count != want[i].Count || p.Value != want[i].Value {
				t.Fatalf("partial input loss pass %d: %+v want %+v", pass, p, want[i])
			}
		}
		if n == 0 {
			break
		}
		if pass == 49 {
			t.Fatal("promotion did not drain")
		}
	}
	if n, err := e.PromoteSummariesChunk(ctx, now, 512); err != nil || n != 0 {
		t.Fatal("not idempotent", n, err)
	}
	// Late observations in the sealed range own another segment, and remain additive.
	late := TelemetryBatch{Source: "node", Identity: "late", Points: []Point{{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(time.Second), Value: 100}}}
	if _, err := e.Accept(ctx, late, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	promoteTelemetryAll(t, e, now.Add(time.Hour))
	r, err := e.Chart(ctx, q, now.Add(time.Hour))
	if err != nil || len(r.Points) != 2 || r.Points[0].Count != 91 || r.Points[0].Value != want[0].Value+100 {
		t.Fatalf("late supplement: %+v %v", r, err)
	}
}

func TestTelemetryTiersSixHourChartRetains360MinuteBuckets(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-6 * time.Hour)
	for part := 0; part < 2; part++ {
		b := TelemetryBatch{Source: "node", Identity: fmt.Sprintf("six-hours/%d", part)}
		for i := part * 180; i < (part+1)*180; i++ {
			b.Points = append(b.Points, Point{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(time.Duration(i) * time.Minute), Value: float64(i)})
		}
		if _, err := e.Accept(ctx, b, now); err != nil {
			t.Fatal(err)
		}
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	promoteTelemetryAll(t, e, now)
	for {
		n, err := e.PruneSummaries(ctx, "cpu", 30*24*time.Hour, now, 512)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	r, err := e.Chart(ctx, AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: now.Add(-time.Nanosecond)}, Interval: time.Minute, Aggregation: AggAvg, PreserveSeries: true}, now)
	if err != nil || len(r.Points) != 360 || len(r.Gaps) > 0 {
		t.Fatalf("six-hour minute coverage: got %d buckets, %d gaps; want 360 exact buckets: %v", len(r.Points), len(r.Gaps), err)
	}
}

func promoteTelemetryAll(t *testing.T, e *Telemetry, now time.Time) {
	t.Helper()
	for {
		n, err := e.PromoteSummariesChunk(context.Background(), now, 128)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

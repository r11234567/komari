package metric

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func testTelemetryPackedGapExports(t *testing.T, e *Telemetry, now time.Time) {
	t.Helper()
	ctx := WithTelemetryEntities(context.Background(), []string{"gap-node"})
	at := now.Add(-time.Hour)
	b := TelemetryBatch{Source: "gap-node", Identity: "gap-packing"}
	for i := 0; i < 80; i++ {
		b.Points = append(b.Points, Point{MetricName: "cpu", EntityID: "gap-node", Timestamp: at.Add(time.Duration(i)*10*time.Second + 4*time.Second), Value: float64(i)})
	}
	if _, err := e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	var segment, series string
	if err := e.s.db.QueryRowContext(ctx, e.bind("SELECT m.segment_id,s.id FROM "+e.table("members")+" m JOIN "+e.table("series")+" s ON s.id=m.series_id WHERE s.entity=?"), "gap-node").Scan(&segment, &series); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 80; i++ {
		first := at.Add(time.Duration(i)*10*time.Second + 2*time.Second).UnixNano()
		if _, err := e.s.db.ExecContext(ctx, e.bind("INSERT INTO "+e.table("gaps")+" (segment_id,series_id,first_observed,last_observed) VALUES (?,?,?,?)"), segment, series, first, first+int64(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := gapSemanticHash(t, e)
	firstPass := true
	cursor := ""
	for {
		n, next, err := e.CompactGapsChunk(ctx, cursor, 17)
		if err != nil || n > 17 || len(next) > 255 {
			t.Fatal("gap page", n, next, err)
		}
		cursor = next
		if firstPass {
			firstPass = false
			// Restart the engine between pages while retaining the durable key.
			if err = e.Close(); err != nil {
				t.Fatal(err)
			}
			e, err = e.s.OpenTelemetry(ctx, e.cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
		}
		if n == 0 {
			break
		}
	}
	if after, _ := gapSemanticHash(t, e); after != before {
		t.Fatal("exact missing intervals changed")
	}
	for i := 0; i < 80; i++ {
		start := at.Add(time.Duration(i)*10*time.Second + 4*time.Second)
		q := Query{MetricName: "cpu", EntityID: "gap-node", Start: start, End: start.Add(time.Second)}
		got, err := e.ExportRawBatch(ctx, []Query{q})
		if err != nil || len(got[0]) != 1 || got[0][0].Value != float64(i) {
			t.Fatal("valid raw between gaps rejected", i, got, err)
		}
		if i%40 == 0 || i == 79 {
			dir := t.TempDir()
			snapshot, err := e.FreezeExport(ctx, []Query{q}, dir, 16<<20)
			if err != nil {
				t.Fatal("worker snapshot rejected exact gap-free range", err)
			}
			count := 0
			for unit := range snapshot.Units {
				if err = VisitExportInput(ctx, dir, snapshot, unit, func(p ExportObservation) error {
					count++
					if p.Point.Value != float64(i) {
						t.Fatal("snapshot returned outside observation", p)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if count != 1 {
				t.Fatal("snapshot lost gap-free observation", count)
			}
		}
		q.Start = start.Add(-2 * time.Second)
		q.End = start.Add(-time.Second)
		if _, err = e.ExportRawBatch(ctx, []Query{q}); !errors.Is(err, ErrTelemetryCoverage) {
			t.Fatal("actual missing raw accepted", i, err)
		}
		if i%40 == 0 {
			if _, err = e.FreezeExport(ctx, []Query{q}, t.TempDir(), 16<<20); !errors.Is(err, ErrTelemetryCoverage) {
				t.Fatal("snapshot accepted genuinely missing raw", err)
			}
		}
	}
}

func TestTelemetryPackedGapsPreserveRawExportWindows(t *testing.T) {
	_, e := telemetryFixture(t, "")
	testTelemetryPackedGapExports(t, e, time.Now().UTC().Truncate(time.Hour))
}

func gapSemanticHash(t *testing.T, e *Telemetry) (string, int) {
	t.Helper()
	r, err := e.s.db.Query("SELECT segment_id,series_id,first_observed,last_observed,ranges FROM " + e.table("gaps") + " ORDER BY segment_id,series_id,first_observed")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	h := sha256.New()
	count := 0
	for r.Next() {
		var seg, sid, packed string
		var first, last int64
		if err = r.Scan(&seg, &sid, &first, &last, &packed); err != nil {
			t.Fatal(err)
		}
		ranges, err := decodeTelemetryGaps(first, last, packed)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range ranges {
			fmt.Fprintf(h, "%s/%s/%d/%d\n", seg, sid, v.First, v.Last)
			count++
		}
	}
	if err = r.Err(); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil)), count
}

func TestTelemetryGapCodecAndTransactionRollback(t *testing.T) {
	ranges := []telemetryGapRange{{10, 10}, {15, 20}, {30, 40}}
	if got, err := decodeTelemetryGaps(10, 40, encodeTelemetryGaps(ranges)); err != nil || !reflect.DeepEqual(got, ranges) {
		t.Fatal(got, err)
	}
	for _, s := range []string{"TG2:", "TG1:AAAA", "TG1:!", encodeTelemetryGaps([]telemetryGapRange{{10, 10}})} {
		if _, err := decodeTelemetryGaps(10, 40, s); err == nil {
			t.Fatal("corrupt ranges accepted", s)
		}
	}
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := e.s.db.ExecContext(ctx, e.bind("INSERT INTO "+e.table("gaps")+" (segment_id,series_id,first_observed,last_observed) VALUES ('segment','series',?,?)"), i*10, i*10+1); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := gapSemanticHash(t, e)
	e.cfg.Fault = func(stage string) error {
		if stage == "gaps-before-compaction-commit" {
			return errors.New("injected crash")
		}
		return nil
	}
	if _, _, err := e.CompactGapsChunk(ctx, "", 512); err == nil {
		t.Fatal("fault not reached")
	}
	if after, _ := gapSemanticHash(t, e); after != before || telemetryCount(t, e, "gaps") != 3 {
		t.Fatal("failed compaction changed source")
	}
}

func TestTelemetryGapCompactionIncidentCopy(t *testing.T) {
	dir := os.Getenv("KOMARI_TELEMETRY_COMPACTION_COPY")
	if dir == "" {
		t.Skip("disposable incident copy not supplied")
	}
	if _, err := os.Stat(filepath.Join(dir, "DISPOSABLE-CAPACITY-COPY")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	s, err := Open(ctx, SQLite(filepath.Join(dir, "metrics.db"), WithTablePrefix("metric_"), WithAutoMigrate(false), WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e, err := s.OpenTelemetry(ctx, TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	before, count := gapSemanticHash(t, e)
	rowsBefore := telemetryCount(t, e, "gaps")
	cursor := ""
	start := time.Now()
	for {
		n, next, err := e.CompactGapsChunk(ctx, cursor, 128)
		if err != nil {
			t.Fatal(err)
		}
		cursor = next
		if n == 0 {
			break
		}
	}
	after, countAfter := gapSemanticHash(t, e)
	if before != after || count != countAfter {
		t.Fatal("exact incident gaps changed")
	}
	result := map[string]any{"seconds": time.Since(start).Seconds(), "rows_before": rowsBefore, "rows_after": telemetryCount(t, e, "gaps"), "exact_intervals": count, "canonical_sha256_before": before, "canonical_sha256_after": after}
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "gap-compaction.json"), append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log(string(b))
}

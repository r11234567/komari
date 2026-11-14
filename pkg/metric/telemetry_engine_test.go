package metric

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func telemetryFixture(t *testing.T, dir string) (*Store, *Telemetry) {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	s, err := Open(context.Background(), SQLite(filepath.Join(dir, "metrics.db"), WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	// Fault/lifecycle fixtures explicitly use a short handoff period. Runtime
	// defaults and sustained volume are covered by the lifecycle regression.
	e, err := s.OpenTelemetry(context.Background(), TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive"), SegmentDuration: 15 * time.Minute})
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close(); s.Close() })
	return s, e
}

func telemetryBatch(identity string, at time.Time, values ...float64) TelemetryBatch {
	b := TelemetryBatch{Source: "node", Identity: identity}
	for i, v := range values {
		b.Points = append(b.Points, Point{EntityID: "node", MetricName: "cpu", Timestamp: at.Add(time.Duration(i) * time.Millisecond), Value: v, Tags: map[string]string{"device": "0"}})
	}
	return b
}

func telemetryCount(t *testing.T, e *Telemetry, table string) int64 {
	t.Helper()
	var n int64
	if err := e.s.db.QueryRow("SELECT COUNT(*) FROM " + e.table(table)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func summarizeAll(t *testing.T, e *Telemetry) {
	t.Helper()
	for {
		n, err := e.SummarizeNext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
}

func TestTelemetryDurableAcceptance(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	dir := t.TempDir()
	s, e := telemetryFixture(t, dir)
	b := telemetryBatch("epoch1/1", now, 1, 2, 3)
	failure := errors.New("injected I/O failure")
	e.cfg.Fault = func(stage string) error {
		if stage == "ingest-before-commit" {
			return failure
		}
		return nil
	}
	if _, err := e.Accept(ctx, b, now); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	for _, table := range []string{"hot", "receipts", "outbox", "segments", "series"} {
		if n := telemetryCount(t, e, table); n != 0 {
			t.Fatalf("partial transaction %s=%d", table, n)
		}
	}
	e.cfg.Fault = func(stage string) error {
		if stage == "ingest-after-commit" {
			return failure
		}
		return nil
	}
	if _, err := e.Accept(ctx, b, now); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	e.Close()
	s.Close()
	s, e = telemetryFixture(t, dir)
	r, err := e.Accept(ctx, b, now.Add(time.Second))
	if err != nil || !r.Duplicate || r.IngestID != 1 || r.Points != 3 {
		t.Fatalf("durable retry %+v %v", r, err)
	}
	b.Points[0].Value = 9
	if r, err := e.Accept(ctx, b, now); err != nil || !r.Duplicate {
		t.Fatal(r, err)
	}
	if n := telemetryCount(t, e, "hot"); n != 3 {
		t.Fatal(n)
	}
	// A process restart can reuse its sequence with a different observed time.
	if _, err = e.Accept(ctx, telemetryBatch("epoch2/1", now.Add(time.Second), 4), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	s.db.SetConnMaxLifetime(time.Nanosecond)
	var synchronous int
	if err = s.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("replacement connection durability=%d err=%v", synchronous, err)
	}
}

func TestTelemetryConcurrentRetryAndValidation(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	b := telemetryBatch("one", now, 1)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := e.Accept(ctx, b, now); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := telemetryCount(t, e, "receipts"); n != 1 {
		t.Fatal(n)
	}
	bad := []TelemetryBatch{telemetryBatch("nan", now, math.NaN()), telemetryBatch("old", now.Add(-49*time.Hour), 1), telemetryBatch("future", now.Add(time.Hour), 1)}
	b = telemetryBatch("wrong-node", now, 1)
	b.Points[0].EntityID = "other"
	bad = append(bad, b)
	for _, b := range bad {
		if _, err := e.Accept(ctx, b, now); err == nil {
			t.Fatalf("accepted %+v", b)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.Accept(cancelled, telemetryBatch("cancel", now, 2), now); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if n := telemetryCount(t, e, "receipts"); n != 1 {
		t.Fatal(n)
	}
}

func TestTelemetrySummaryWeightedStatisticsAndSketchIsolation(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Hour)
	now := start.Add(time.Hour)
	var raw []Point
	for batch := range 9 {
		values := make([]float64, 1+batch*7)
		for i := range values {
			values[i] = float64((batch*7 + i) % 19)
			if batch == 0 {
				values[i] = 100
			}
		}
		b := telemetryBatch(fmt.Sprint(batch), start.Add(time.Duration(batch)*10*time.Second), values...)
		raw = append(raw, b.Points...)
		if _, err := e.Accept(ctx, b, now); err != nil {
			t.Fatal(err)
		}
	}
	e.cfg.Fault = func(stage string) error {
		if stage == "summary-before-commit" {
			return errors.New("commit failed")
		}
		return nil
	}
	if _, err := e.SummarizeNext(ctx); err == nil {
		t.Fatal("missing fault")
	}
	if n := telemetryCount(t, e, "summary"); n != 0 {
		t.Fatal(n)
	}
	e.cfg.Fault = nil
	summarizeAll(t, e)
	for _, agg := range []Aggregation{AggAvg, AggSum, AggMin, AggMax, AggCount, AggLast} {
		q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: start, End: start.Add(5*time.Minute - time.Nanosecond)}, Interval: 5 * time.Minute, Aggregation: agg, PreserveSeries: true}
		got, err := e.Aggregate(ctx, q, now)
		if err != nil {
			t.Fatal(err)
		}
		want, err := AggregatePoints(raw, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Points) != 1 || len(want) != 1 {
			t.Fatalf("%s: %+v vs %+v", agg, got, want)
		}
		tolerance := 1e-9
		if isPercentile(agg) {
			tolerance = 1
		}
		if math.Abs(got.Points[0].Value-want[0].Value) > tolerance || got.Points[0].Count != len(raw) {
			t.Fatalf("%s got %+v want %+v", agg, got.Points[0], want[0])
		}
	}
	if telemetryCount(t, e, "summary") != 0 {
		t.Fatal("legacy statistics written")
	}
}

func TestTelemetryArchiveFaultRecoveryAndDeletionBarriers(t *testing.T) {
	for _, stage := range []string{"archive-prepared", "archive-page-written", "archive-before-sync", "archive-before-rename", "archive-after-rename", "archive-before-catalog", "archive-after-catalog"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s, e := telemetryFixture(t, dir)
			now := time.Now().UTC().Truncate(time.Hour)
			b := telemetryBatch("one", now, 1, 2, 3)
			b.Traffic = &TelemetryTraffic{TotalUp: 123, TotalDown: 456}
			if _, err := e.Accept(ctx, b, now); err != nil {
				t.Fatal(err)
			}
			if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			e.cfg.Fault = func(s string) error {
				if s == stage {
					return errors.New("crash at " + stage)
				}
				return nil
			}
			if _, err := e.ArchiveNext(ctx); err == nil {
				t.Fatal("missing fault")
			}
			if n := telemetryCount(t, e, "hot"); n != 3 {
				t.Fatal(n)
			}
			e.Close()
			s.Close()
			_, e = telemetryFixture(t, dir)
			if _, err := e.ArchiveNext(ctx); err != nil {
				t.Fatal(err)
			}
			if n, err := e.DeleteArchivedChunk(ctx, now.Add(time.Hour), 2); err != nil || n != 0 {
				t.Fatalf("deleted before summaries %d %v", n, err)
			}
			summarizeAll(t, e)
			if n, err := e.DeleteArchivedChunk(ctx, now.Add(time.Hour), 2); err != nil || n != 0 {
				t.Fatalf("deleted before traffic handoff %d %v", n, err)
			}
			applied := map[int64]bool{}
			total := int64(0)
			apply := func(_ context.Context, event TelemetryTrafficEvent) error {
				if !applied[event.IngestID] {
					applied[event.IngestID] = true
					total += event.Traffic.TotalUp
				}
				return nil
			}
			e.cfg.Fault = func(s string) error {
				if s == "outbox-after-consumer" {
					return errors.New("lost acknowledgement")
				}
				return nil
			}
			if _, err := e.DeliverTraffic(ctx, 1, apply); err == nil {
				t.Fatal("missing delivery fault")
			}
			e.cfg.Fault = nil
			if _, err := e.DeliverTraffic(ctx, 1, apply); err != nil || total != 123 {
				t.Fatalf("duplicate traffic %d %v", total, err)
			}
			// No time-based raw retention remains after all handoff barriers.
			e.cfg.Fault = func(s string) error {
				if s == "delete-before-commit" {
					return errors.New("cancel deletion")
				}
				return nil
			}
			if _, err := e.DeleteArchivedChunk(ctx, now.Add(time.Hour), 2); err == nil {
				t.Fatal("missing delete fault")
			}
			if n := telemetryCount(t, e, "hot"); n != 3 {
				t.Fatal(n)
			}
			e.cfg.Fault = nil
			if n, err := e.DeleteArchivedChunk(ctx, now.Add(time.Hour), 2); err != nil || n != 2 {
				t.Fatalf("first chunk %d %v", n, err)
			}
			if n, err := e.DeleteArchivedChunk(ctx, now.Add(time.Hour), 2); err != nil || n != 1 {
				t.Fatalf("last chunk %d %v", n, err)
			}
			var id string
			if err := e.s.db.QueryRow("SELECT id FROM " + e.table("segments")).Scan(&id); err != nil {
				t.Fatal(err)
			}
			seg, err := e.segment(ctx, e.s.db, id)
			if err != nil {
				t.Fatal(err)
			}
			var values []float64
			if err = e.visitArchive(ctx, seg, func(r telemetryRecord) error { values = append(values, r.Point.Value); return nil }); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(values, []float64{1, 2, 3}) || seg.State != "complete" {
				t.Fatalf("bad archive %v %+v", values, seg)
			}
		})
	}
}

func TestTelemetryArchiveCorruptionRetainsSource(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := e.Accept(ctx, telemetryBatch("one", now, 1, 2), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	e.SealDue(ctx, now.Add(time.Hour))
	if _, err := e.ArchiveNext(ctx); err != nil {
		t.Fatal(err)
	}
	var id string
	e.s.db.QueryRow("SELECT id FROM " + e.table("segments")).Scan(&id)
	seg, err := e.segment(ctx, e.s.db, id)
	if err != nil {
		t.Fatal(err)
	}
	f, err := e.root.OpenFile(e.segmentPath(seg), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = 0, f.Truncate(seg.FileBytes-1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err = e.DeleteArchivedChunk(ctx, now.Add(time.Hour), 512); err == nil {
		t.Fatal("corrupt archive allowed deletion")
	}
	if n := telemetryCount(t, e, "hot"); n != 2 {
		t.Fatal(n)
	}
}

func TestTelemetryArchiveLeaseFencesAnExpiredWorker(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	other, err := s.OpenTelemetry(ctx, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err = e.Accept(ctx, telemetryBatch("lease", now, 1, 2), now); err != nil {
		t.Fatal(err)
	}
	e.SealDue(ctx, now.Add(time.Hour))
	if err = e.claimWork(ctx, "archive"); err != nil {
		t.Fatal(err)
	}
	if _, err = other.ArchiveNext(ctx); !errors.Is(err, ErrTelemetryBackpressure) {
		t.Fatalf("second writer admitted: %v", err)
	}
	e.releaseWork("archive")
	e.cfg.Fault = func(stage string) error {
		if stage == "archive-page-written" {
			e.releaseWork("archive")
			return other.claimWork(ctx, "archive")
		}
		return nil
	}
	if _, err = e.ArchiveNext(ctx); !errors.Is(err, ErrTelemetryBackpressure) {
		t.Fatalf("expired worker published: %v", err)
	}
	if n := telemetryCount(t, e, "hot"); n != 2 {
		t.Fatal(n)
	}
	if ok, err := other.ArchiveNext(ctx); err != nil || !ok {
		t.Fatalf("replacement worker failed %v %v", ok, err)
	}
}

func TestTelemetryUnknownSQLSchemaFailsClosed(t *testing.T) {
	s, e := telemetryFixture(t, "")
	if _, err := s.db.Exec("UPDATE " + e.table("progress") + " SET value=99 WHERE task='schema_version'"); err != nil {
		t.Fatal(err)
	}
	if other, err := s.OpenTelemetry(context.Background(), e.cfg); err == nil {
		other.Close()
		t.Fatal("opened unknown SQL schema")
	}
	if n := telemetryCount(t, e, "receipts"); n != 0 {
		t.Fatal(n)
	}
}

func TestTelemetryLateSupplementAndRetentionNeverReadCold(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Hour)
	for i, v := range []float64{100, 0} {
		b := telemetryBatch(fmt.Sprint(i), start.Add(time.Second), v)
		if _, err := e.Accept(ctx, b, start.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
		summarizeAll(t, e)
		if _, err := e.SealDue(ctx, start.Add(time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.ArchiveNext(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := e.DeleteArchivedChunk(ctx, start.Add(time.Duration(i+1)*time.Hour), 512); err != nil {
			t.Fatal(err)
		}
	}
	// Removing read access to all archive files cannot affect chart SQL.
	promoteTelemetryAll(t, e, start.Add(4*time.Hour))
	if err := e.root.Close(); err != nil {
		t.Fatal(err)
	}
	for {
		n, err := e.PruneSummaries(ctx, "cpu", 30*24*time.Hour, start.Add(4*time.Hour), 512)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: start, End: start.Add(5*time.Minute - time.Nanosecond)}, Interval: 5 * time.Minute, Aggregation: AggAvg, PreserveSeries: true}
	got, err := e.Aggregate(ctx, q, start.Add(3*time.Hour))
	if err != nil || len(got.Points) != 1 || got.Points[0].Value != 50 || got.Points[0].Count != 2 {
		t.Fatalf("late summary %+v %v", got, err)
	}
	var fine int
	if err = e.s.db.QueryRow("SELECT COUNT(*) FROM " + e.table("summary") + " WHERE resolution=10000000000").Scan(&fine); err != nil || fine != 0 {
		t.Fatalf("unused precision retained %d %v", fine, err)
	}
}

func TestTelemetryReceiptExpiryAndBackpressure(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC()
	b := telemetryBatch("receipt", at, 1, 2)
	if _, err := e.Accept(ctx, b, at); err != nil {
		t.Fatal(err)
	}
	if n, err := e.PruneReceipts(ctx, at.Add(72*time.Hour), 512); err != nil || n != 0 {
		t.Fatalf("pruned unarchived %d %v", n, err)
	}
	summarizeAll(t, e)
	e.SealDue(ctx, at.Add(time.Hour))
	if _, err := e.ArchiveNext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DeleteArchivedChunk(ctx, at.Add(time.Hour), 512); err != nil {
		t.Fatal(err)
	}
	if n, err := e.PruneReceipts(ctx, at.Add(time.Hour), 512); err != nil || n != 1 {
		t.Fatalf("completed handoff receipt %d %v", n, err)
	}
	if n, err := e.PruneReceipts(ctx, at.Add(72*time.Hour), 512); err != nil || n != 0 {
		t.Fatalf("expired receipt %d %v", n, err)
	}
	if _, err := e.Accept(ctx, b, at.Add(72*time.Hour)); err == nil {
		t.Fatal("reapplied expired historical receipt")
	}
	e.cfg.MaxHotBytes = 1
	if _, err := e.Accept(ctx, telemetryBatch("new", at, 3), at); !errors.Is(err, ErrTelemetryBackpressure) {
		t.Fatal(err)
	}
	if n := telemetryCount(t, e, "receipts"); n != 0 {
		t.Fatal(n)
	}
}

func TestTelemetryProcessRoundTrip(t *testing.T) {
	if os.Getenv("KOMARI_TELEMETRY_PROCESS_HELPER") == "1" {
		telemetryProcessHelper(t)
		return
	}
	dir := t.TempDir()
	for _, phase := range []string{"ingest", "crash", "archive", "verify"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestTelemetryProcessRoundTrip$", "-test.v")
		cmd.Env = append(os.Environ(), "KOMARI_TELEMETRY_PROCESS_HELPER=1", "KOMARI_TELEMETRY_PROCESS_DIR="+dir, "KOMARI_TELEMETRY_PROCESS_PHASE="+phase)
		if out, err := cmd.CombinedOutput(); err != nil {
			if phase == "crash" {
				var exit *exec.ExitError
				if errors.As(err, &exit) && exit.ExitCode() == 88 {
					continue
				}
			}
			t.Fatalf("%s: %v\n%s", phase, err, out)
		}
		if phase == "crash" {
			t.Fatal("process fault did not exit")
		}
	}
}

func telemetryProcessHelper(t *testing.T) {
	_, e := telemetryFixture(t, os.Getenv("KOMARI_TELEMETRY_PROCESS_DIR"))
	ctx := context.Background()
	at := time.Now().UTC().Truncate(24 * time.Hour)
	b := telemetryBatch("process/one", at, 1, 2, 3)
	switch os.Getenv("KOMARI_TELEMETRY_PROCESS_PHASE") {
	case "ingest":
		if r, err := e.Accept(ctx, b, at); err != nil || r.Duplicate {
			t.Fatalf("%+v %v", r, err)
		}
	case "crash":
		e.cfg.WorkLease = 20 * time.Millisecond
		e.cfg.Fault = func(stage string) error {
			if stage == "archive-after-rename" {
				os.Exit(88)
			}
			return nil
		}
		summarizeAll(t, e)
		e.SealDue(ctx, at.Add(time.Hour))
		e.ArchiveNext(ctx)
		t.Fatal("missing process crash")
	case "archive":
		if r, err := e.Accept(ctx, b, at); err != nil || !r.Duplicate {
			t.Fatalf("%+v %v", r, err)
		}
		summarizeAll(t, e)
		e.SealDue(ctx, at.Add(time.Hour))
		deadline := time.Now().Add(2 * time.Second)
		for {
			ok, err := e.ArchiveNext(ctx)
			if errors.Is(err, ErrTelemetryBackpressure) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if err != nil || !ok {
				t.Fatalf("%v %v", ok, err)
			}
			break
		}
		if n, err := e.DeleteArchivedChunk(ctx, at.Add(time.Hour), 512); err != nil || n != 3 {
			t.Fatalf("%d %v", n, err)
		}
	case "verify":
		if n := telemetryCount(t, e, "hot"); n != 0 {
			t.Fatal(n)
		}
		var id string
		e.s.db.QueryRow("SELECT id FROM " + e.table("segments")).Scan(&id)
		seg, err := e.segment(ctx, e.s.db, id)
		if err != nil {
			t.Fatal(err)
		}
		var values []float64
		if err = e.visitArchive(ctx, seg, func(r telemetryRecord) error { values = append(values, r.Point.Value); return nil }); err != nil {
			t.Fatal(err)
		}
		sort.Float64s(values)
		if !reflect.DeepEqual(values, []float64{1, 2, 3}) {
			t.Fatal(values)
		}
	default:
		t.Fatal("unknown process phase")
	}
}

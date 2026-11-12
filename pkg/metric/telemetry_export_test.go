package metric

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestTelemetryCancelDuringColdDecodeReleasesLease(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := WithTelemetryEntities(context.Background(), []string{"node"})
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-time.Hour)
	values := make([]float64, 256)
	for i := range values {
		values[i] = float64(i)
	}
	if _, err := e.Accept(ctx, telemetryBatch("cancel-cold", at, values...), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.ArchiveNext(ctx); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if n, err := e.DeleteArchivedChunk(ctx, now.Add(2*time.Hour), 512); err != nil || n != len(values) {
		t.Fatal(n, err)
	}
	q := Query{MetricName: "cpu", EntityID: "node", Start: at, End: now}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	decoding := false
	run = WithTelemetryWorkYield(run, func(context.Context) error {
		// Called after 128 real archive records have been decoded, with a
		// committed export lease. This is not a pre-cancelled request.
		decoding = true
		if n := telemetryCount(t, e, "leases"); n != 1 {
			t.Fatalf("active leases = %d", n)
		}
		cancel()
		return run.Err()
	})
	began := time.Now()
	if _, err := e.ExportRawBatch(run, []Query{q}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !decoding || time.Since(began) > time.Second {
		t.Fatal("cold decode did not stop promptly", decoding, time.Since(began))
	}
	if n := telemetryCount(t, e, "leases"); n != 0 {
		t.Fatalf("cancel leaked %d archive leases", n)
	}
	got, err := e.ExportRawBatch(ctx, []Query{q})
	if err != nil || len(got[0]) != len(values) {
		t.Fatal("subsequent cold export", err, len(got))
	}
}

func TestTelemetryDictionaryErasureSurvivesSharedColdFileAndReindex(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := WithTelemetryEntities(context.Background(), []string{"node"})
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-time.Hour)
	b := telemetryBatch("shared-file", at, 2, 3)
	b.Points[1].Tags = map[string]string{"device": "1"}
	if _, err := e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.ArchiveNext(ctx); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := e.DeleteArchivedChunk(ctx, now.Add(time.Hour), 512); err != nil {
		t.Fatal(err)
	}
	var segment string
	if err := e.s.db.QueryRow("SELECT id FROM " + e.table("segments") + " LIMIT 1").Scan(&segment); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.Exec("DELETE FROM " + e.table("members")); err != nil {
		t.Fatal(err)
	}
	if err := e.IndexLegacyCoreSegment(ctx, segment); err != nil {
		t.Fatal(err)
	}
	if _, err := e.EraseSeries(ctx, Query{MetricName: "cpu", EntityID: "node", Tags: map[string]string{"device": "0"}}); err != nil {
		t.Fatal(err)
	}
	queries := []Query{{MetricName: "cpu", EntityID: "node", Start: at, End: now}}
	points, err := e.ExportRawBatch(ctx, queries)
	if err != nil || len(points[0]) != 1 || points[0][0].Value != 3 {
		t.Fatal("erased canonical identity resurfaced", points, err)
	}
	dir := filepath.Join(t.TempDir(), "snapshot")
	snapshot, err := e.FreezeExport(ctx, queries, dir, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	frozen := exportSnapshotPoints(t, dir, snapshot)
	if len(frozen) != 1 || frozen[0].Point.Value != 3 {
		t.Fatal("frozen export resurfaced erased identity", frozen)
	}
}

func TestTelemetryExportHotColdOwnershipAndLease(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := WithTelemetryEntities(context.Background(), []string{"node"})
	now := time.Now().UTC().Truncate(time.Minute)
	at := now.Add(-time.Hour)
	b := telemetryBatch("first", at, 1, 3, 8)
	if _, err := e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	q := Query{MetricName: "cpu", EntityID: "node", Start: at, End: now, Order: OrderAsc}
	want, err := e.ExportRawBatch(ctx, []Query{q})
	if err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err = e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.ArchiveNext(ctx); err != nil || !ok {
		t.Fatal(ok, err)
	}
	e.cfg.Fault = func(stage string) error {
		if stage == "export-pinned" {
			if n, err := e.DeleteArchivedChunk(ctx, now.Add(2*time.Hour), 512); err != nil || n != 0 {
				t.Fatalf("export lease lost: %d %v", n, err)
			}
		}
		return nil
	}
	got, err := e.ExportRawBatch(ctx, []Query{q})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("archive + still hot %+v %v want %+v", got, err, want)
	}
	e.cfg.Fault = nil
	if n, err := e.DeleteArchivedChunk(ctx, now.Add(2*time.Hour), 512); err != nil || n != 3 {
		t.Fatal(n, err)
	}
	got, err = e.ExportRawBatch(ctx, []Query{q})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("archive only %+v %v want %+v", got, err, want)
	}
	if _, err = e.ExportRawBatch(context.Background(), []Query{q}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unscoped export: %v", err)
	}
	if n, err := e.EraseSeries(ctx, Query{EntityID: "node"}); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	got, err = e.ExportRawBatch(ctx, []Query{q})
	if err != nil || len(got[0]) != 0 {
		t.Fatalf("erased archive resurfaced: %+v %v", got, err)
	}
	b = telemetryBatch("after-delete", now, 13)
	if _, err = e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	got, err = e.ExportRawBatch(ctx, []Query{q})
	if err != nil || len(got[0]) != 1 || got[0][0].Value != 13 {
		t.Fatalf("fresh sample %+v %v", got, err)
	}
}

func TestTelemetryErasePendingInputDoesNotRecreateSummary(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	b := telemetryBatch("before", now, 1, 2)
	if _, err := e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.EraseSeries(ctx, Query{EntityID: "node"}); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: now, End: now.Add(time.Minute - time.Nanosecond)}, Interval: time.Minute, Aggregation: AggSum, PreserveSeries: true}
	if got, err := e.Chart(ctx, q, now); err != nil || len(got.Points) != 0 {
		t.Fatalf("pending erased input %+v %v", got, err)
	}
	b = telemetryBatch("after", now.Add(time.Second), 7)
	if _, err := e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if got, err := e.Chart(ctx, q, now); err != nil || len(got.Points) != 1 || got.Points[0].Value != 7 {
		t.Fatalf("new summary %+v %v", got, err)
	}
	if n, err := e.PruneErasedChunk(ctx, 512); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

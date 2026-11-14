package metric

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTelemetryRetentionCycleKeepsMissingHistoryExplicit(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	if err := s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Accept(ctx, telemetryBatch("history", now, 1, 2, 3), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.ArchiveNext(ctx); err != nil || !ok {
		t.Fatal(ok, err)
	}
	for {
		n, err := e.DeleteArchivedChunk(ctx, now.Add(time.Hour), 2)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	// Within chart retention the catalog cannot disappear, even if the raw
	// file expires under a shorter independently configured policy.
	if ok, err := e.ExpireArchiveNext(ctx, now.Add(3*24*time.Hour), 24*time.Hour); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if n, err := e.PruneCatalogChunk(ctx, now.Add(3*24*time.Hour), 128); err != nil || n != 0 {
		t.Fatal("catalog pruned before chart history", n, err)
	}
	late := now.Add(40 * 24 * time.Hour)
	if _, err := e.PruneSummaries(ctx, "cpu", 30*24*time.Hour, late, 512); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PruneReceipts(ctx, late, 512); err != nil {
		t.Fatal(err)
	}
	if n, err := e.PruneCatalogChunk(ctx, late, 128); err != nil || n == 0 {
		t.Fatal("expired catalog never reclaimed", n, err)
	}
	for _, table := range []string{"hot", "summary", "receipts", "members", "segments", "series", "series_keys"} {
		if n := telemetryCount(t, e, table); n != 0 {
			t.Fatal(table, n)
		}
	}
	_, err := e.ExportRawBatch(WithTelemetryEntities(ctx, []string{"node"}), []Query{{MetricName: "cpu", EntityID: "node", Start: now, End: now.Add(time.Minute)}})
	if !errors.Is(err, ErrTelemetryCoverage) {
		t.Fatal("GC turned expired raw into successful empty CSV", err)
	}
}

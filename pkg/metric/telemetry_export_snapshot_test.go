package metric

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func exportTestQueries(at time.Time) []Query {
	return []Query{{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute)}}
}
func exportSnapshotPoints(t *testing.T, dir string, snapshot ExportSnapshot) []ExportObservation {
	t.Helper()
	var out []ExportObservation
	ctx := WithTelemetryEntities(context.Background(), []string{"node"})
	for i := range snapshot.Units {
		if e := VisitExportInput(ctx, dir, snapshot, i, func(o ExportObservation) error { out = append(out, o); return nil }); e != nil {
			t.Fatal(e)
		}
	}
	return out
}
func TestTelemetryExportSnapshotFramesHotColdAndFixedWatermark(t *testing.T) {
	s, tel := telemetryFixture(t, "")
	ctx := WithTelemetryEntities(context.Background(), []string{"node"})
	at := time.Now().UTC().Add(-time.Hour)
	if e := s.UpsertMetric(ctx, Definition{Name: "cpu", Unit: "percent", RetentionDays: 30}); e != nil {
		t.Fatal(e)
	}
	values := make([]float64, 300)
	for i := range values {
		values[i] = float64(i)
	}
	if _, e := tel.Accept(ctx, telemetryBatch("cold-1", at, values[:200]...), at); e != nil {
		t.Fatal(e)
	}
	if _, e := tel.Accept(ctx, telemetryBatch("cold-2", at.Add(200*time.Millisecond), values[200:]...), at); e != nil {
		t.Fatal(e)
	}
	if _, e := tel.SealDue(ctx, time.Now().Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e := tel.ArchiveNext(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e := tel.Accept(ctx, telemetryBatch("hot", at.Add(time.Second), 301), time.Now()); e != nil {
		t.Fatal(e)
	}
	tel.cfg.Fault = func(stage string) error {
		if stage == "export-snapshot-pinned" {
			_, e := tel.Accept(ctx, telemetryBatch("late-after-watermark", at.Add(2*time.Second), 999), time.Now())
			return e
		}
		return nil
	}
	dir := filepath.Join(t.TempDir(), "snapshot")
	snapshot, e := tel.FreezeExport(ctx, exportTestQueries(at), dir, 10<<20)
	if e != nil {
		t.Fatal(e)
	}
	if len(snapshot.Units) != 2 {
		t.Fatalf("byte-sized archive and hot-tail units: %d", len(snapshot.Units))
	}
	if n := telemetryCount(t, tel, "leases"); n != 0 {
		t.Fatalf("long export pinned live retention: %d", n)
	}
	points := exportSnapshotPoints(t, dir, snapshot)
	if len(points) != 301 {
		t.Fatalf("fixed snapshot lost/duplicated or included late ingress: %d", len(points))
	}
	seen := map[string]bool{}
	for _, p := range points {
		if seen[p.Identity] {
			t.Fatal("duplicate identity", p.Identity)
		}
		seen[p.Identity] = true
		if p.Unit != "percent" || p.Point.Value == 999 {
			t.Fatal("incorrect unit/watermark", p)
		}
	}
	// The frozen files retain ownership even when live files are removed.
	if e = os.RemoveAll(tel.cfg.ArchiveDirectory); e != nil {
		t.Fatal(e)
	}
	if got := len(exportSnapshotPoints(t, dir, snapshot)); got != 301 {
		t.Fatal(got)
	}
	// Checkpoints for a framed source read only that frame, not its siblings.
	var decoded, scanned int64
	if e = VisitExportInputProgress(ctx, dir, snapshot, 0, func(n, b int64) { decoded += n; scanned += b }, func(ExportObservation) error { return nil }); e != nil {
		t.Fatal(e)
	}
	if decoded > 300 || scanned != snapshot.Units[0].Segment.FileBytes {
		t.Fatalf("frame read amplification: %d %d", decoded, scanned)
	}
}
func TestTelemetryExportSnapshotScopeErasureFloorAndLeaseCleanup(t *testing.T) {
	_, tel := telemetryFixture(t, "")
	at := time.Now().UTC().Add(-time.Hour)
	ctx := WithTelemetryEntities(context.Background(), []string{"node"})
	b := telemetryBatch("one", at, 1)
	b.Points[0].Tags = nil
	if _, e := tel.Accept(ctx, b, time.Now()); e != nil {
		t.Fatal(e)
	}
	if _, e := tel.FreezeExport(context.Background(), exportTestQueries(at), t.TempDir(), 1<<20); !errors.Is(e, ErrInvalidArgument) {
		t.Fatal("missing scope accepted", e)
	}
	empty, e := tel.FreezeExport(WithTelemetryEntities(ctx, []string{}), exportTestQueries(at), t.TempDir(), 1<<20)
	if e != nil || len(empty.Units) != 0 {
		t.Fatal("unauthorized scope leaked", empty, e)
	}
	tel.cfg.Fault = func(stage string) error {
		if stage == "export-snapshot-pinned" {
			return errors.New("interrupted snapshot")
		}
		return nil
	}
	if _, e = tel.FreezeExport(ctx, exportTestQueries(at), t.TempDir(), 1<<20); e == nil {
		t.Fatal("fault did not fail")
	}
	if n := telemetryCount(t, tel, "leases"); n != 0 {
		t.Fatal("failed freeze leaked leases", n)
	}
	tel.cfg.Fault = nil
	if _, e = tel.EraseSeries(ctx, Query{EntityID: "node", MetricName: "cpu"}); e != nil {
		t.Fatal(e)
	}
	snap, e := tel.FreezeExport(ctx, exportTestQueries(at), t.TempDir(), 1<<20)
	if e != nil || len(snap.Units) != 0 {
		t.Fatal("erased series exported", snap, e)
	}
	if _, e = tel.s.db.Exec("INSERT INTO "+tel.table("progress")+"(task,value) VALUES ('raw_history_floor',?)", at.Add(time.Second).UnixNano()); e != nil {
		t.Fatal(e)
	}
	if _, e = tel.FreezeExport(ctx, exportTestQueries(at), t.TempDir(), 1<<20); !errors.Is(e, ErrTelemetryCoverage) {
		t.Fatal("expired raw silently substituted", e)
	}
}
func TestTelemetryExportSnapshotRejectsMissingMembershipAndCorruptFrame(t *testing.T) {
	_, tel := telemetryFixture(t, "")
	at := time.Now().UTC().Add(-time.Hour)
	ctx := WithTelemetryEntities(context.Background(), []string{"node"})
	if _, e := tel.Accept(ctx, telemetryBatch("one", at, 1, 2), time.Now()); e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	snap, e := tel.FreezeExport(ctx, exportTestQueries(at), dir, 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(dir, snap.Units[0].File)
	b, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	b[20] ^= 1
	if e = os.WriteFile(file, b, 0600); e != nil {
		t.Fatal(e)
	}
	if e = VisitExportInput(ctx, dir, snap, 0, func(ExportObservation) error { return nil }); e == nil {
		t.Fatal("corrupt frame accepted")
	}
	if _, e = tel.s.db.Exec("DELETE FROM " + tel.table("members")); e != nil {
		t.Fatal(e)
	}
	if _, e = tel.FreezeExport(ctx, exportTestQueries(at), t.TempDir(), 1<<20); !errors.Is(e, ErrTelemetryCoverage) {
		t.Fatal("missing index silently lost raw", e)
	}
}
func TestTelemetryArchiveFrameIndexCorruptionIsRejected(t *testing.T) {
	_, tel := telemetryFixture(t, "")
	at := time.Now().UTC().Add(-time.Hour)
	if _, e := tel.Accept(context.Background(), telemetryBatch("one", at, 1, 2), at); e != nil {
		t.Fatal(e)
	}
	if _, e := tel.SealDue(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if _, e := tel.ArchiveNext(context.Background()); e != nil {
		t.Fatal(e)
	}
	var id string
	if e := tel.s.db.QueryRow("SELECT id FROM " + tel.table("segments") + " WHERE state='archived'").Scan(&id); e != nil {
		t.Fatal(e)
	}
	seg, e := tel.segment(context.Background(), tel.s.db, id)
	if e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(tel.cfg.ArchiveDirectory, tel.segmentPath(seg))
	b, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	b[len(b)-16] = 255
	h := sha256.Sum256(b)
	seg.Hash = hex.EncodeToString(h[:])
	if e = os.WriteFile(file, b, 0600); e != nil {
		t.Fatal(e)
	}
	if e = tel.visitArchive(context.Background(), seg, nil); e == nil {
		t.Fatal("invalid frame index accepted despite recomputed whole-file hash")
	}
}

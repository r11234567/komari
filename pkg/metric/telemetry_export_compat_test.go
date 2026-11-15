package metric

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestTelemetryExportReadsLegacySingleStreamWithoutRewriting(t *testing.T) {
	_, tel := telemetryFixture(t, "")
	ctx := WithTelemetryEntities(context.Background(), []string{"node"})
	at := time.Now().UTC().Add(-time.Hour)
	if _, e := tel.Accept(ctx, telemetryBatch("legacy-single", at, 10, 20, 30), at); e != nil {
		t.Fatal(e)
	}
	if _, e := tel.SealDue(ctx, time.Now().Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e := tel.ArchiveNext(ctx); e != nil {
		t.Fatal(e)
	}
	var id string
	if e := tel.s.db.QueryRow("SELECT id FROM " + tel.table("segments") + " WHERE state='archived'").Scan(&id); e != nil {
		t.Fatal(e)
	}
	seg, e := tel.segment(ctx, tel.s.db, id)
	if e != nil {
		t.Fatal(e)
	}
	var records []telemetryRecord
	if e = tel.visitArchive(ctx, seg, func(r telemetryRecord) error { records = append(records, r); return nil }); e != nil {
		t.Fatal(e)
	}
	var plain bytes.Buffer
	enc := json.NewEncoder(&plain)
	if e = enc.Encode(telemetryRecord{Kind: "header", Version: 1, Segment: id}); e != nil {
		t.Fatal(e)
	}
	for _, r := range records {
		if e = enc.Encode(r); e != nil {
			t.Fatal(e)
		}
	}
	content := sha256.Sum256(plain.Bytes())
	if e = enc.Encode(telemetryRecord{Kind: "footer", Count: int64(len(records)), Hash: hex.EncodeToString(content[:])}); e != nil {
		t.Fatal(e)
	}
	encoder, e := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if e != nil {
		t.Fatal(e)
	}
	compressed := encoder.EncodeAll(plain.Bytes(), nil)
	encoder.Close()
	sum := sha256.Sum256(compressed)
	source := filepath.Join(tel.cfg.ArchiveDirectory, tel.segmentPath(seg))
	if e = os.WriteFile(source, compressed, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = tel.s.db.Exec("UPDATE "+tel.table("segments")+" SET checksum=?,file_bytes=? WHERE id=?", hex.EncodeToString(sum[:]), len(compressed), id); e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(t.TempDir(), "snapshot")
	snapshot, e := tel.FreezeExport(ctx, exportTestQueries(at), dir, 10<<20)
	if e != nil {
		t.Fatal(e)
	}
	if len(snapshot.Units) != 1 || snapshot.Units[0].Frame != nil {
		t.Fatalf("legacy archive unexpectedly rewritten: %+v", snapshot.Units)
	}
	if got := len(exportSnapshotPoints(t, dir, snapshot)); got != 3 {
		t.Fatal(got)
	}
	after, e := os.ReadFile(source)
	if e != nil || !bytes.Equal(compressed, after) {
		t.Fatal("source archive modified", e)
	}
	frozen, e := os.ReadFile(filepath.Join(dir, snapshot.Units[0].File))
	if e != nil || !bytes.Equal(compressed, frozen) {
		t.Fatal("frozen legacy archive rewritten", e)
	}
}

func TestTelemetryExportEpochChangesAtomicallyWithHistoryErasure(t *testing.T) {
	_, tel := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().Add(-time.Minute)
	if _, e := tel.Accept(ctx, telemetryBatch("epoch", at, 10), at); e != nil {
		t.Fatal(e)
	}
	before, e := tel.ExportState(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tel.EraseSeries(ctx, Query{MetricName: "cpu", EntityID: "node"}); e != nil {
		t.Fatal(e)
	}
	after, e := tel.ExportState(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if after.Instance != before.Instance || after.Epoch <= before.Epoch {
		t.Fatal("erasure failed to invalidate frozen exports", before, after)
	}
}

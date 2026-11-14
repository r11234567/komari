package metric

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func lifecycleArchiveAll(t *testing.T, e *Telemetry, now time.Time) {
	t.Helper()
	ctx := context.Background()
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, now.Add(e.cfg.SegmentDuration)); err != nil {
		t.Fatal(err)
	}
	for {
		ok, err := e.ArchiveNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
	}
	for {
		n, err := e.DeleteArchivedChunk(ctx, now, 128)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
}

func TestTelemetryLiveWindowSurvivesRolloverRestartAndExpiry(t *testing.T) {
	dir := t.TempDir()
	s, e := telemetryFixture(t, dir)
	s.telemetry.Store(e)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(10 * time.Second)
	e.cfg.SegmentDuration = time.Minute
	for i := 0; i < 15; i++ {
		at := now.Add(time.Duration(i-14) * time.Minute)
		if _, err := e.Accept(ctx, telemetryBatch(fmt.Sprint(i), at, float64(i)), at); err != nil {
			t.Fatal(err)
		}
	}
	q := Query{MetricName: "cpu", EntityID: "node", Start: now.Add(-145 * time.Minute / 10), End: now}
	before, err := e.ReadHot(ctx, q, now)
	if err != nil || len(before) != 15 {
		t.Fatal(len(before), err)
	}
	lifecycleArchiveAll(t, e, now)
	after, err := e.ReadHot(ctx, q, now)
	if err != nil || len(after) != len(before) {
		t.Fatal("rollover truncated live history", len(after), err)
	}
	for i := range before {
		if before[i].Value != after[i].Value || !before[i].Timestamp.Equal(after[i].Timestamp) {
			t.Fatal("live population changed")
		}
	}
	if pending, err := e.progress(ctx, s.db, "hot_bytes"); err != nil || pending != 0 {
		t.Fatal(pending, err)
	}
	var detached int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM " + e.table("hot") + " WHERE segment_id=''").Scan(&detached); err != nil || detached != 15 {
		t.Fatal(detached, err)
	}
	latest, err := s.Latest(ctx, "cpu", "node", 1)
	if err != nil || len(latest) != 1 || latest[0].Value != 14 {
		t.Fatal(latest, err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, e = telemetryFixture(t, dir)
	s.telemetry.Store(e)
	after, err = e.ReadHot(ctx, q, now)
	if err != nil || len(after) != 15 {
		t.Fatal("restart lost live history", len(after), err)
	}
	for {
		n, err := e.DeleteArchivedChunk(ctx, now.Add(16*time.Minute), 4)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if n := telemetryCount(t, e, "hot"); n != 0 {
		t.Fatal("live rows did not expire", n)
	}
	if n, err := e.progress(ctx, s.db, "live_bytes"); err != nil || n != 0 {
		t.Fatal("live budget leaked", n, err)
	}
	latest, err = s.Latest(ctx, "cpu", "node", 1)
	if err != nil || len(latest) != 1 || latest[0].Value != 14 {
		t.Fatal("idle latest vanished", latest, err)
	}
	t.Logf("rollover/restart retained %d raw observations; expiry reclaimed every live row and byte; offline latest retained", len(after))
}

func TestTelemetryLatestHistoricalBoundaryAndCSVOnlyRetirement(t *testing.T) {
	s, e := telemetryFixture(t, "")
	s.telemetry.Store(e)
	ctx := context.Background()
	now := time.Now().UTC()
	at := now.Add(-3 * time.Hour).Truncate(time.Minute)
	b := telemetryBatch("last", at.Add(10*time.Second), 1)
	b.Points = append(b.Points, Point{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(50 * time.Second), Value: 9, Tags: map[string]string{"device": "0"}})
	if _, err := e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"traffic.up", "traffic.down"} {
		b := telemetryBatch(name, now, 3)
		b.Points[0].MetricName = name
		if _, err := e.Accept(ctx, b, now); err != nil {
			t.Fatal(err)
		}
	}
	lifecycleArchiveAll(t, e, now)
	if n := telemetryCount(t, e, "hot"); n != 0 {
		t.Fatal("CSV-only rows retained", n)
	}
	p, ok, err := s.LatestBefore(ctx, "cpu", "node", at.Add(time.Minute))
	if err != nil || !ok || p.Value != 9 || !p.Timestamp.Equal(at.Add(50*time.Second)) {
		t.Fatal(p, ok, err)
	}
	if _, _, err = s.LatestBefore(ctx, "cpu", "node", at.Add(30*time.Second)); !errors.Is(err, ErrTelemetryCoverage) {
		t.Fatal("partial historical last invented", err)
	}
	// Out-of-order observations cannot roll the last snapshot backwards.
	if _, err = e.Accept(ctx, telemetryBatch("older", at, 77), now); err != nil {
		t.Fatal(err)
	}
	latest, err := s.Latest(ctx, "cpu", "node", 1)
	if err != nil || len(latest) != 1 || latest[0].Value != 9 {
		t.Fatal(latest, err)
	}
	if _, err = e.EraseSeries(ctx, Query{MetricName: "cpu", EntityID: "node"}); err != nil {
		t.Fatal(err)
	}
	latest, err = s.Latest(ctx, "cpu", "node", 1)
	if err != nil || len(latest) != 0 {
		t.Fatal("erasure resurrected latest", latest, err)
	}
}

func TestTelemetryHistoricalSlidingDenseGrid(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	e.cfg.MaxLateness = 400 * 24 * time.Hour
	for _, age := range []time.Duration{30 * time.Hour, 4 * 24 * time.Hour, 20 * 24 * time.Hour, 90 * 24 * time.Hour} {
		start := alignTime(now.Add(-age), 24*time.Hour)
		b := TelemetryBatch{Source: "node", Identity: age.String()}
		// Dense points cross requested 10s/4m grids after promotion.
		for i := 0; i < 120; i++ {
			b.Points = append(b.Points, Point{MetricName: "cpu", EntityID: "node", Timestamp: start.Add(time.Duration(i) * time.Minute), Value: float64(i)})
		}
		if _, err := e.Accept(ctx, b, now); err != nil {
			t.Fatal(err)
		}
	}
	summarizeAll(t, e)
	promoteTelemetryAll(t, e, now)
	for _, age := range []time.Duration{30 * time.Hour, 4 * 24 * time.Hour, 20 * 24 * time.Hour, 90 * 24 * time.Hour} {
		start := alignTime(now.Add(-age), 24*time.Hour)
		q := scalarQuery(start, 24*time.Hour, 10*time.Second, "cpu")
		var err error
		q.Interval, err = e.ChartInterval(ctx, q.Query, now, TelemetryChartInterval(24*time.Hour, 600))
		if err != nil {
			t.Fatal(err)
		}
		r, err := e.Chart(ctx, q, now)
		if err != nil || len(r.Points) == 0 || len(r.Gaps) != 0 {
			t.Fatal(age, q.Interval, len(r.Points), r.Gaps, err)
		}
		var n int64
		var sum float64
		for _, p := range r.Points {
			n += int64(p.Count)
			sum += p.Value * float64(p.Count)
		}
		if n != 120 || sum != 7140 {
			t.Fatal("sliding window changed population", age, n, sum)
		}
		t.Logf("historical window age=%s interval=%s population=%d", age, q.Interval, n)
	}
	if got := TelemetryChartInterval(24*time.Hour, 600); got != 5*time.Minute {
		t.Fatal("unstable daily grid", got)
	}
}

func TestTelemetrySharedArchiveRecoverySnapshotBackupAndExpiry(t *testing.T) {
	dir := t.TempDir()
	s, e := telemetryFixture(t, dir)
	ctx := WithTelemetryEntities(context.Background(), []string{"node"})
	now := time.Now().UTC()
	at := now.Add(-3 * time.Hour).Truncate(time.Hour)
	for i := 0; i < 2; i++ {
		if _, err := e.Accept(ctx, telemetryBatch(fmt.Sprint(i), at.Add(time.Duration(i)*time.Minute), float64(i)), at.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
		summarizeAll(t, e)
		if _, err := e.SealDue(ctx, at.Add(time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			e.cfg.Fault = func(stage string) error {
				if stage == "archive-before-catalog" {
					return errors.New("publication interrupted")
				}
				return nil
			}
		}
		_, err := e.ArchiveNext(ctx)
		if i == 1 {
			if err == nil {
				t.Fatal("missing injected interruption")
			}
			e.cfg.Fault = nil
			if _, err = e.ArchiveNext(ctx); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	for {
		n, err := e.DeleteArchivedChunk(ctx, now, 128)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	var files, segments int
	if err := s.db.QueryRow("SELECT COUNT(DISTINCT archive_file),COUNT(*) FROM "+e.table("segments")+" WHERE state='complete'").Scan(&files, &segments); err != nil || files != 1 || segments != 2 {
		t.Fatal(files, segments, err)
	}
	queries := []Query{{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(2 * time.Minute)}}
	points, err := e.ExportRawBatch(ctx, queries)
	if err != nil || len(points[0]) != 2 {
		t.Fatal(points, err)
	}
	frozen := filepath.Join(t.TempDir(), "frozen")
	snapshot, err := e.FreezeExport(ctx, queries, frozen, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(exportSnapshotPoints(t, frozen, snapshot)); got != 2 {
		t.Fatal(got)
	}
	var id string
	if err = s.db.QueryRow("SELECT id FROM " + e.table("segments") + " ORDER BY first_id LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	first, err := e.segment(ctx, s.db, id)
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := os.ReadFile(filepath.Join(e.cfg.ArchiveDirectory, e.segmentPath(first)))
	if err != nil {
		t.Fatal(err)
	}
	// Appending later data must not change the frozen export or any old prefix.
	if _, err = e.Accept(ctx, telemetryBatch("later", at.Add(2*time.Minute), 2), now); err != nil {
		t.Fatal(err)
	}
	lifecycleArchiveAll(t, e, now)
	current, err := os.ReadFile(filepath.Join(e.cfg.ArchiveDirectory, e.segmentPath(first)))
	if err != nil || !bytes.HasPrefix(current, prefix) {
		t.Fatal("published prefix rewritten", err)
	}
	if got := len(exportSnapshotPoints(t, frozen, snapshot)); got != 2 {
		t.Fatal("frozen snapshot followed append", got)
	}
	backup := t.TempDir()
	dbpath := filepath.Join(backup, "metrics.db")
	archive := filepath.Join(backup, "archive")
	if err = e.BackupBundle(ctx, dbpath, archive, nil); err != nil {
		t.Fatal(err)
	}
	if err = ValidateTelemetryBackup(ctx, dbpath, archive); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(archive, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest telemetryBackupManifest
	if err = json.Unmarshal(b, &manifest); err != nil || len(manifest.Files) != 1 {
		t.Fatal("backup duplicated shared file", len(manifest.Files), err)
	}
	// The first unit expires while the others still own the same file.
	if ok, err := e.ExpireArchiveNext(ctx, at.Add(30*24*time.Hour+30*time.Minute), 30*24*time.Hour); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err = os.Stat(filepath.Join(e.cfg.ArchiveDirectory, e.segmentPath(first))); err != nil {
		t.Fatal("expired unit removed siblings", err)
	}
	for {
		ok, err := e.ExpireArchiveNext(ctx, now.Add(40*24*time.Hour), 30*24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
	}
	if _, err = os.Stat(filepath.Join(e.cfg.ArchiveDirectory, e.segmentPath(first))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unowned shared archive leaked", err)
	}
}

func TestTelemetryCapacitySealsLargestPartitionOnly(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	e.cfg.SegmentDuration = 24 * time.Hour
	for node := 0; node < 8; node++ {
		b := telemetryBatch(fmt.Sprint(node), now, 1)
		b.Source = fmt.Sprint(node)
		b.Points[0].EntityID = b.Source
		if _, err := e.Accept(ctx, b, now); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := e.SealDue(ctx, now.Add(16*time.Minute)); err != nil || n != 0 {
		t.Fatal("quarter-hour tiny files returned", n, err)
	}
	if _, err := e.s.db.Exec("UPDATE "+e.table("progress")+" SET value=? WHERE task='hot_bytes'", e.cfg.MaxHotBytes*4/5); err != nil {
		t.Fatal(err)
	}
	if n, err := e.SealDue(ctx, now); err != nil || n != 1 {
		t.Fatal("capacity pressure sealed every tiny partition", n, err)
	}
}

func TestTelemetryHistoricalImportDayDoesNotSealEveryPage(t *testing.T) {
	_, e := telemetryFixture(t, "")
	e.cfg.SegmentDuration = 24 * time.Hour
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	old := now.Add(-30 * 24 * time.Hour).Truncate(24 * time.Hour)
	for page := 0; page < 8; page++ {
		b := telemetryBatch(fmt.Sprint(page), old.Add(time.Duration(page)*time.Hour), float64(page))
		if _, err := e.accept(ctx, b, now, true); err != nil {
			t.Fatal(err)
		}
		if n, err := e.SealDue(ctx, now); err != nil || n != 0 {
			t.Fatal("historical age sealed import page", page, n, err)
		}
	}
	if n := telemetryCount(t, e, "segments"); n != 1 {
		t.Fatal("import fragmented one machine/day", n)
	}
}

// This runs the normal online lifecycle against a growing SQLite database;
// it does not VACUUM or manufacture the progress counters for a small result.
func TestTelemetrySustainedLifecycle(t *testing.T) {
	if os.Getenv("KOMARI_TELEMETRY_SUSTAINED") != "1" {
		t.Skip("opt-in sustained disk regression")
	}
	ctx := context.Background()
	dir := t.TempDir()
	s, e := telemetryFixture(t, dir)
	e.cfg.SegmentDuration = 24 * time.Hour
	base := time.Now().UTC().Add(-36 * time.Hour).Truncate(10 * time.Minute)
	var peak int64
	for batch := 0; batch < 4320; batch++ {
		end := base.Add(time.Duration(batch+1) * 30 * time.Second)
		for node := 0; node < 3; node++ {
			b := TelemetryBatch{Source: fmt.Sprint(node), Identity: fmt.Sprint(batch)}
			for metric := 0; metric < 8; metric++ {
				b.Points = append(b.Points, Point{MetricName: fmt.Sprintf("chart.%d", metric), EntityID: fmt.Sprint(node), Timestamp: end, Value: float64(batch)})
			}
			if _, err := e.Accept(ctx, b, end); err != nil {
				t.Fatal(batch, err)
			}
		}
		if batch%2 == 0 {
			continue
		} // Ordinary maintenance cadence: once a minute.
		summarizeAll(t, e)
		promoteTelemetryAll(t, e, end)
		if _, err := e.SealDue(ctx, end); err != nil {
			t.Fatal(err)
		}
		for {
			ok, err := e.ArchiveNext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
		}
		for {
			n, err := e.DeleteArchivedChunk(ctx, end, 512)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
		}
		for {
			n, err := e.PruneReceipts(ctx, end, 512)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
		}
		pending, err := e.progress(ctx, s.db, "hot_bytes")
		if err != nil {
			t.Fatal(err)
		}
		live, err := e.progress(ctx, s.db, "live_bytes")
		if err != nil || pending+live > e.cfg.MaxHotBytes {
			t.Fatal(batch, pending, live, err)
		}
		q := Query{MetricName: "chart.0", EntityID: "0", Start: end.Add(-145 * time.Minute / 10), End: end}
		points, err := e.ReadHot(ctx, q, end)
		want := min(batch+1, 30)
		if err != nil || len(points) != want {
			t.Fatal("live sawtooth", batch, len(points), want, err)
		}
		var disk int64
		for _, suffix := range []string{"", "-wal", "-shm"} {
			info, err := os.Stat(filepath.Join(dir, "metrics.db") + suffix)
			if err == nil {
				disk += info.Size()
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
		peak = max(peak, disk)
	}
	var files, segments int
	if err := s.db.QueryRow("SELECT COUNT(DISTINCT archive_file),COUNT(*) FROM "+e.table("segments")+" WHERE archive_file<>''").Scan(&files, &segments); err != nil {
		t.Fatal(err)
	}
	if files == 0 || files > 9 || files >= segments {
		t.Fatal("physical archive fragmentation", files, segments)
	}
	if peak > 50<<20 {
		t.Fatal("sustained SQL/WAL/SHM exceeded scenario disk budget", peak)
	}
	t.Logf("36h, 3 nodes, 8 metrics, 30s reports: 12960 ingest transactions, 103680 observations; peak SQL+WAL+SHM=%d; physical archives=%d logical units=%d", peak, files, segments)
}

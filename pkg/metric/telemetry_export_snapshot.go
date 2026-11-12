package metric

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ExportSnapshot is a private immutable manifest, never an HTTP response. Raw
// input is frozen once under a short lease; hour-long formatting pins no source
// retention, writer transaction or application lifecycle lease.
type ExportSnapshot struct {
	Version       int
	Instance      string
	Watermark     int64
	Epoch         int64
	Queries       []Query
	Units         []ExportInput
	Erased        map[string]int64
	UnitsByMetric map[string]string
	Summary       []TelemetryQueryResult
	Bytes         int64
}
type ExportInput struct {
	Segment telemetrySegment
	File    string
	Frame   *telemetryArchiveFrame `json:",omitempty"`
}
type ExportObservation struct {
	Point    Point
	Ingest   int64
	Ordinal  int
	Received time.Time
	Identity string
	Unit     string
}

// FreezeExport copies only hot membership and links immutable archives. The
// caller must use a generated, private directory and run in the export cgroup.
// Unavailable history fails explicitly; it is never substituted with rollups.
func (t *Telemetry) FreezeExport(ctx context.Context, queries []Query, dir string, maxBytes int64) (out ExportSnapshot, err error) {
	if len(queries) == 0 || len(queries) > 64 || maxBytes < 1 {
		return out, ErrInvalidArgument
	}
	if _, ok := ctx.Value(telemetryScopeKey{}).([]string); !ok {
		return out, ErrInvalidArgument
	}
	for _, q := range queries {
		if err = q.Validate(); err != nil {
			return out, err
		}
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return out, err
	}
	start, end := queries[0].Start, queries[0].End
	for _, q := range queries {
		start = minTime(start, q.Start)
		end = maxTelemetryTime(end, q.End)
	}
	lease := telemetryID()
	tx, err := t.begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	watermark, err := t.progress(ctx, tx, "ingest")
	if err != nil {
		return out, err
	}
	var floor int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(value),0) FROM "+t.table("progress")+" WHERE task='raw_history_floor'").Scan(&floor); err != nil {
		return out, err
	}
	if floor > 0 && start.UnixNano() < floor {
		return out, fmt.Errorf("%w: original observations have expired; select summary mode explicitly", ErrTelemetryCoverage)
	}
	out = ExportSnapshot{Version: 1, Instance: t.instance, Watermark: watermark, Queries: queries, Erased: map[string]int64{}, UnitsByMetric: map[string]string{}}
	out.Epoch, err = t.progress(ctx, tx, "export_epoch")
	if err != nil {
		return out, err
	}
	// Read only explicitly authorized erasures. A fixed manifest includes the
	// generation and tombstones from the same transaction as its watermark.
	for _, q := range queries {
		p := q
		if p.MetricName == sqliteVirtualPingLossMetric {
			p.MetricName = sqliteMergedPingLatencyMetric
		}
		filter, args, e := t.seriesFilter(ctx, p, "s")
		if e != nil {
			return out, e
		}
		rows, e := tx.QueryContext(ctx, t.bind("SELECT e.series_id,e.through_id FROM "+t.table("erasures")+" e JOIN "+t.table("series")+" s ON s.id=e.series_id WHERE s.metric=?"+filter+" LIMIT 16385"), append([]any{p.MetricName}, args...)...)
		if e != nil {
			return out, e
		}
		for rows.Next() {
			var id string
			var through int64
			if e = rows.Scan(&id, &through); e != nil {
				break
			}
			out.Erased[id] = through
			if len(out.Erased) > 16384 {
				e = ErrTelemetryBackpressure
				break
			}
		}
		re := rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
		if re != nil {
			return out, re
		}
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, e := t.s.db.ExecContext(c, t.bind("DELETE FROM "+t.table("leases")+" WHERE id=?"), lease)
		if err == nil {
			err = e
		}
	}()
	segments, err := t.reserveExportSegments(ctx, queries, watermark, lease, start)
	if err != nil {
		return out, err
	}
	if err = t.fault("export-snapshot-pinned"); err != nil {
		return out, err
	}
	definitions, err := t.s.ListMetrics(ctx)
	if err != nil {
		return out, err
	}
	for _, d := range definitions {
		out.UnitsByMetric[d.Name] = d.Unit
	}
	sort.Slice(segments, func(i, j int) bool {
		if segments[i].First == segments[j].First {
			return segments[i].ID < segments[j].ID
		}
		return segments[i].First < segments[j].First
	})
	deadline := time.Now().Add(9 * time.Minute)
	for _, seg := range segments {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		if time.Now().After(deadline) {
			return out, errors.New("snapshot reservation exceeded 9 minutes; split export range")
		}
		// A lease makes SQL deletion and archive expiry ineligible. An open
		// segment can still accept newer reports, filtered by fixed watermark.
		name := seg.ID + ".jsonl.zst"
		target := filepath.Join(dir, name)
		switch seg.State {
		case "archived", "deleting", "complete":
			info, e := t.root.Lstat(t.segmentPath(seg))
			if e != nil {
				return out, e
			}
			if !info.Mode().IsRegular() || info.Size() != seg.FileBytes {
				return out, errors.New("snapshot archive unavailable")
			}
			if out.Bytes+seg.FileBytes > maxBytes {
				return out, errors.New("export input disk budget exceeded")
			}
			// The immutable file has a server-derived name in a private archive
			// root. Hardlinks freeze ownership without decoding or copying it.
			if e = os.Link(filepath.Join(t.cfg.ArchiveDirectory, t.segmentPath(seg)), target); e != nil {
				if e = t.copyExportArchive(ctx, seg, target); e != nil {
					return out, e
				}
			}
		case "open", "sealed", "writing":
			seg, err = t.freezeHotSegment(ctx, seg, watermark, target, maxBytes-out.Bytes)
			if err != nil {
				return out, err
			}
		default:
			return out, ErrTelemetryCoverage
		}
		out.Bytes += seg.FileBytes
		if out.Bytes > maxBytes {
			return out, errors.New("export input disk budget exceeded")
		}
		f, e := os.Open(target)
		if e != nil {
			return out, e
		}
		index, e := readTelemetryArchiveIndex(f, seg)
		if e == nil && index != nil {
			e = verifyTelemetryArchiveFile(ctx, f, seg)
		}
		f.Close()
		if e != nil {
			return out, e
		}
		if index == nil {
			if len(out.Units) >= 16384 {
				return out, ErrTelemetryBackpressure
			}
			out.Units = append(out.Units, ExportInput{Segment: seg, File: name})
		} else {
			for _, frame := range index.Frames {
				if len(out.Units) >= 16384 {
					return out, ErrTelemetryBackpressure
				}
				part := seg
				part.Points, part.First, part.Last, part.FileBytes, part.Hash = frame.Points, frame.First, frame.Last, frame.Bytes, frame.Hash
				copy := frame
				out.Units = append(out.Units, ExportInput{Segment: part, File: name, Frame: &copy})
			}
		}
	}
	return out, nil
}

// Each reservation transaction pins at most 32 segments. CPU throttling and
// snapshot file copying happen outside writer transactions. Recheck the raw
// retention floor after pinning so concurrent expiry cannot silently lose data.
func (t *Telemetry) reserveExportSegments(ctx context.Context, queries []Query, watermark int64, lease string, start time.Time) ([]telemetrySegment, error) {
	var missing string
	e := t.s.reader().QueryRowContext(ctx, "SELECT a.id FROM "+t.table("segments")+" a WHERE a.points>0 AND a.state<>'expired' AND NOT EXISTS (SELECT 1 FROM "+t.table("members")+" m WHERE m.segment_id=a.id) LIMIT 1").Scan(&missing)
	if e == nil {
		return nil, fmt.Errorf("%w: archive membership indexing pending", ErrTelemetryCoverage)
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	selected := map[string]telemetrySegment{}
	for _, q := range queries {
		physical := q
		if physical.MetricName == sqliteVirtualPingLossMetric {
			physical.MetricName = sqliteMergedPingLatencyMetric
		}
		filter, args, e := t.seriesFilter(ctx, physical, "s")
		if e != nil {
			return nil, e
		}
		gapArgs := append([]any{physical.MetricName, q.End.UnixNano(), q.Start.UnixNano()}, args...)
		hasGap, e := t.exportHasLegacyGap(ctx, t.s.reader(), "s.metric=? AND g.first_observed<=? AND g.last_observed>=? AND (e.through_id IS NULL OR a.last_id>e.through_id)"+filter, gapArgs, q.Start.UnixNano(), q.End.UnixNano())
		if e != nil {
			return nil, e
		}
		if hasGap {
			return nil, fmt.Errorf("%w: legacy raw was already removed; select summary mode", ErrTelemetryCoverage)
		}
		after := ""
		for {
			tx, e := t.begin(ctx)
			if e != nil {
				return nil, e
			}
			params := append([]any{physical.MetricName, q.End.UnixNano(), q.Start.UnixNano(), watermark, after}, args...)
			rows, e := tx.QueryContext(ctx, t.bind("SELECT DISTINCT m.segment_id FROM "+t.table("members")+" m JOIN "+t.table("series")+" s ON s.id=m.series_id JOIN "+t.table("segments")+" a ON a.id=m.segment_id LEFT JOIN "+t.table("erasures")+" e ON e.series_id=s.id WHERE s.metric=? AND m.first_observed<=? AND m.last_observed>=? AND a.first_id<=? AND a.id>? AND a.state NOT IN ('legacy','expired') AND (e.through_id IS NULL OR a.last_id>e.through_id)"+filter+" ORDER BY m.segment_id LIMIT 32"), params...)
			if e != nil {
				tx.Rollback()
				return nil, e
			}
			var ids []string
			for rows.Next() {
				var id string
				if e = rows.Scan(&id); e != nil {
					break
				}
				ids = append(ids, id)
			}
			re := rows.Err()
			rows.Close()
			if e != nil {
				tx.Rollback()
				return nil, e
			}
			if re != nil {
				tx.Rollback()
				return nil, re
			}
			for _, id := range ids {
				if _, ok := selected[id]; ok {
					continue
				}
				if len(selected) >= 20000 {
					tx.Rollback()
					return nil, ErrTelemetryBackpressure
				}
				seg, e := t.segment(ctx, tx, id)
				if e != nil {
					tx.Rollback()
					return nil, e
				}
				if _, e = tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("leases")+" (id,segment_id,expires) VALUES (?,?,?)"), lease, id, time.Now().Add(10*time.Minute).UnixNano()); e != nil {
					tx.Rollback()
					return nil, e
				}
				selected[id] = seg
			}
			if e = tx.Commit(); e != nil {
				return nil, e
			}
			if len(ids) < 32 {
				break
			}
			after = ids[len(ids)-1]
			if e = telemetryYield(ctx); e != nil {
				return nil, e
			}
		}
	}
	var floor int64
	if e = t.s.reader().QueryRowContext(ctx, "SELECT COALESCE(MAX(value),0) FROM "+t.table("progress")+" WHERE task='raw_history_floor'").Scan(&floor); e != nil {
		return nil, e
	}
	if floor > 0 && start.UnixNano() < floor {
		return nil, ErrTelemetryCoverage
	}
	out := make([]telemetrySegment, 0, len(selected))
	for _, seg := range selected {
		out = append(out, seg)
	}
	return out, nil
}

func (t *Telemetry) copyExportArchive(ctx context.Context, seg telemetrySegment, target string) error {
	f, err := t.root.Open(t.segmentPath(seg))
	if err != nil {
		return err
	}
	defer f.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	buffer := make([]byte, 64<<10)
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, e := f.Read(buffer)
		if n > 0 {
			if _, err = out.Write(buffer[:n]); err != nil {
				return err
			}
			if err = telemetryYield(ctx); err != nil {
				return err
			}
		}
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
	}
	return out.Sync()
}

func (t *Telemetry) freezeHotSegment(ctx context.Context, seg telemetrySegment, watermark int64, target string, budget int64) (telemetrySegment, error) {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return seg, err
	}
	defer f.Close()
	compressed := &telemetryHashWriter{w: f, h: sha256.New()}
	frames, err := newTelemetryFrameWriter(compressed, seg.ID)
	if err != nil {
		return seg, err
	}
	defer frames.encoder.Close()
	var count, after int64
	ordinal := -1
	for {
		if err = ctx.Err(); err != nil {
			return seg, err
		}
		page, e := t.segmentPage(ctx, seg.ID, after, ordinal, 128)
		if e != nil {
			return seg, e
		}
		if len(page) == 0 {
			break
		}
		stop := false
		for _, r := range page {
			if r.Ingest > watermark {
				stop = true
				break
			}
			if e = frames.add(r); e != nil {
				return seg, e
			}
			count++
			after, ordinal = r.Ingest, r.Ordinal
		}
		if compressed.n > budget {
			return seg, errors.New("snapshot disk budget exceeded")
		}
		if stop {
			break
		}
		if err = telemetryYield(ctx); err != nil {
			return seg, err
		}
	}
	if err = frames.close(); err != nil {
		return seg, err
	}
	if err = f.Sync(); err != nil {
		return seg, err
	}
	seg.Points = count
	seg.FileBytes = compressed.n
	seg.Hash = hex.EncodeToString(compressed.h.Sum(nil))
	return seg, nil
}

// VisitExportInput decodes exactly one immutable source unit in stable ingest /
// ordinal order. It does not collect/sort the complete time range in memory.
func VisitExportInput(ctx context.Context, dir string, snapshot ExportSnapshot, index int, visit func(ExportObservation) error) error {
	return VisitExportInputProgress(ctx, dir, snapshot, index, nil, visit)
}

// progress reports actual decoding and compressed reads, including records
// filtered out of the requested scope and necessary recovery rereads. Neither
// counter is a completion percentage; a unit is complete only after validation.
func VisitExportInputProgress(ctx context.Context, dir string, snapshot ExportSnapshot, index int, progress func(int64, int64), visit func(ExportObservation) error) error {
	if snapshot.Version != 1 || index < 0 || index >= len(snapshot.Units) {
		return ErrInvalidArgument
	}
	unit := snapshot.Units[index]
	if filepath.Base(unit.File) != unit.File || unit.File != unit.Segment.ID+".jsonl.zst" {
		return ErrInvalidArgument
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	reader := &Telemetry{root: root, cfg: TelemetryConfig{SegmentBytes: 1 << 30}}
	metrics := map[string]bool{}
	for _, q := range snapshot.Queries {
		metrics[q.MetricName] = true
	}
	callback := func(r telemetryRecord) error {
		if progress != nil {
			progress(1, 0)
		}
		if r.Point == nil || r.Ingest > snapshot.Watermark {
			return nil
		}
		p := r.Point.normalized()
		if !metrics[p.MetricName] && !(p.MetricName == sqliteMergedPingLatencyMetric && metrics[sqliteVirtualPingLossMetric]) {
			return nil
		}
		encoded, _ := json.Marshal(p.Tags)
		h := sha256.Sum256([]byte(p.MetricName + "\x00" + p.EntityID + "\x00" + string(encoded)))
		if r.Ingest <= snapshot.Erased[hex.EncodeToString(h[:])] {
			return nil
		}
		for _, q := range snapshot.Queries {
			candidate := p
			if q.MetricName == sqliteVirtualPingLossMetric && p.MetricName == sqliteMergedPingLatencyMetric {
				candidate = virtualPingLossPoint(p)
			}
			if !telemetryPointMatches(ctx, candidate, q) || !candidate.Timestamp.Before(q.End) {
				continue
			}
			received := time.Time{}
			if r.Received != 0 {
				received = time.Unix(0, r.Received).UTC()
			}
			if err = visit(ExportObservation{Point: candidate, Ingest: r.Ingest, Ordinal: r.Ordinal, Received: received, Identity: fmt.Sprintf("%s:%d:%d:%s", snapshot.Instance, r.Ingest, r.Ordinal, candidate.MetricName), Unit: snapshot.UnitsByMetric[candidate.MetricName]}); err != nil {
				return err
			}
		}
		return nil
	}
	f, e := root.Open(unit.File)
	if e != nil {
		return e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() {
		return errors.New("invalid export input type")
	}
	var input io.Reader = f
	if unit.Frame == nil {
		if info.Size() != unit.Segment.FileBytes {
			return errors.New("invalid export input size")
		}
	} else {
		if unit.Frame.Offset < 0 || unit.Frame.Bytes != unit.Segment.FileBytes || unit.Frame.Offset > info.Size()-unit.Frame.Bytes {
			return errors.New("invalid export frame extent")
		}
		input = io.NewSectionReader(f, unit.Frame.Offset, unit.Frame.Bytes)
	}
	return reader.visitArchiveStream(ctx, unit.Segment, &exportCountingReader{r: input, progress: progress}, callback)
}

type exportCountingReader struct {
	r        io.Reader
	progress func(int64, int64)
}

func (r *exportCountingReader) Read(b []byte) (int, error) {
	n, e := r.r.Read(b)
	if n > 0 && r.progress != nil {
		r.progress(0, int64(n))
	}
	return n, e
}

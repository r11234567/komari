package metric

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

type telemetryRecord struct {
	Kind     string `json:"kind"`
	Version  int    `json:"schema_version,omitempty"`
	Segment  string `json:"segment_id,omitempty"`
	Ingest   int64  `json:"ingest_id,omitempty"`
	Ordinal  int    `json:"ordinal,omitempty"`
	Received int64  `json:"received_at_ns,omitempty"`
	Point    *Point `json:"point,omitempty"`
	Count    int64  `json:"count,omitempty"`
	Hash     string `json:"sha256,omitempty"`
}

type telemetryHashWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (w *telemetryHashWriter) Write(b []byte) (int, error) {
	n, e := w.w.Write(b)
	if e == nil && n != len(b) {
		e = io.ErrShortWrite
	}
	w.h.Write(b[:n])
	w.n += int64(n)
	return n, e
}

func (t *Telemetry) segmentPath(seg telemetrySegment) string {
	return filepath.Join(time.Unix(0, seg.Created).UTC().Format("2006/01/02"), seg.ID+".jsonl.zst")
}

func (t *Telemetry) segment(ctx context.Context, q querier, id string) (telemetrySegment, error) {
	var s telemetrySegment
	err := q.QueryRowContext(ctx, t.bind("SELECT id,state,first_id,last_id,points,bytes,created,deleted,checksum,file_bytes,staging_owner FROM "+t.table("segments")+" WHERE id=?"), id).Scan(&s.ID, &s.State, &s.First, &s.Last, &s.Points, &s.Bytes, &s.Created, &s.Deleted, &s.Hash, &s.FileBytes, &s.StagingOwner)
	// PostgreSQL pads empty CHAR fields; identities themselves are fixed hex.
	s.StagingOwner = strings.TrimSpace(s.StagingOwner)
	return s, err
}

func (t *Telemetry) segmentPage(ctx context.Context, seg string, ingest int64, ordinal int, limit int) ([]telemetryRecord, error) {
	rows, err := t.s.reader().QueryContext(ctx, t.bind("SELECT h.ingest_id,h.ordinal,h.received,h.observed,h.value,h.labels,s.metric,s.entity,s.tags FROM "+t.table("hot")+" h JOIN "+t.table("series")+" s ON s.id=h.series_id WHERE h.segment_id=? AND (h.ingest_id>? OR (h.ingest_id=? AND h.ordinal>?)) ORDER BY h.ingest_id,h.ordinal LIMIT ?"), seg, ingest, ingest, ordinal, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []telemetryRecord
	for rows.Next() {
		r := telemetryRecord{Kind: "point", Point: &Point{}}
		var ns int64
		var labels, tags string
		if err = rows.Scan(&r.Ingest, &r.Ordinal, &r.Received, &ns, &r.Point.Value, &labels, &r.Point.MetricName, &r.Point.EntityID, &tags); err != nil {
			return nil, err
		}
		r.Point.Timestamp = time.Unix(0, ns).UTC()
		if err = json.Unmarshal([]byte(labels), &r.Point.Labels); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(tags), &r.Point.Tags); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ArchiveNext streams one fixed segment. The writing state survives restart.
// A file becomes readable only after close+fsync+rename+directory fsync, full
// verification and the catalog commit. Retry rewrites only this unregistered
// attempt from still-owned SQL rows, never an already published archive.
func (t *Telemetry) ArchiveNext(ctx context.Context) (bool, error) {
	select {
	case t.archiveGate <- struct{}{}:
		defer func() { <-t.archiveGate }()
	case <-ctx.Done():
		return false, ctx.Err()
	}
	if err := t.claimWork(ctx, "archive"); err != nil {
		return false, err
	}
	defer t.releaseWork("archive")
	var id string
	err := t.s.db.QueryRowContext(ctx, "SELECT id FROM "+t.table("segments")+" WHERE state IN ('sealed','writing') ORDER BY created,id LIMIT 1").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	seg, err := t.segment(ctx, t.s.db, id)
	if err != nil {
		return false, err
	}
	if _, err = t.s.db.ExecContext(ctx, t.bind("UPDATE "+t.table("segments")+" SET state='writing',staging_owner=? WHERE id=? AND state IN ('sealed','writing')"), t.owner, id); err != nil {
		return false, err
	}
	if err = t.fault("archive-prepared"); err != nil {
		return false, err
	}
	path := t.segmentPath(seg)
	dir := filepath.Dir(path)
	if err = t.root.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	// Persist newly created directory entries all the way up to the volume
	// root. Syncing only the leaf would not make its parents crash durable.
	parent := "."
	for _, component := range strings.Split(filepath.ToSlash(dir), "/") {
		d, e := t.root.Open(parent)
		if e != nil {
			return false, e
		}
		e = d.Sync()
		d.Close()
		if e != nil {
			return false, e
		}
		parent = filepath.Join(parent, component)
	}
	part := path + ".part." + t.owner
	if seg.StagingOwner != "" && seg.StagingOwner != t.owner {
		if len(seg.StagingOwner) != 32 {
			return false, errors.New("invalid archive staging identity")
		}
		if _, e := hex.DecodeString(seg.StagingOwner); e != nil {
			return false, e
		}
		if e := t.root.Remove(path + ".part." + seg.StagingOwner); e != nil && !errors.Is(e, os.ErrNotExist) {
			return false, e
		}
	}
	// Only the server-generated segment path is used, beneath os.Root. No HTTP
	// path or catalog-provided arbitrary filename can escape the archive root.
	if info, e := t.root.Lstat(part); e == nil {
		if !info.Mode().IsRegular() {
			return false, errors.New("unsafe archive staging file")
		}
		if err = t.root.Remove(part); err != nil {
			return false, err
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return false, e
	}
	f, err := t.root.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return false, err
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()
	compressed := &telemetryHashWriter{w: f, h: sha256.New()}
	zw, err := zstd.NewWriter(compressed, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<20))
	if err != nil {
		return false, err
	}
	defer zw.Close()
	plain := &telemetryHashWriter{w: zw, h: sha256.New()}
	enc := json.NewEncoder(plain)
	if err = enc.Encode(telemetryRecord{Kind: "header", Version: 1, Segment: id}); err != nil {
		return false, err
	}
	var count, after int64
	ordinal := -1
	for {
		if err = ctx.Err(); err != nil {
			return false, err
		}
		page, e := t.segmentPage(ctx, id, after, ordinal, 128)
		if e != nil {
			return false, e
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			if err = ctx.Err(); err != nil {
				return false, err
			}
			if err = enc.Encode(r); err != nil {
				return false, err
			}
			count++
			after = r.Ingest
			ordinal = r.Ordinal
			if plain.n > t.cfg.SegmentBytes*4 {
				return false, errors.New("telemetry archive exceeded uncompressed budget")
			}
		}
		if err = t.fault("archive-page-written"); err != nil {
			return false, err
		}
		if err = t.renewWork(ctx, t.s.db, "archive"); err != nil {
			return false, err
		}
		if err = telemetryYield(ctx); err != nil {
			return false, err
		}
	}
	if count != seg.Points {
		return false, fmt.Errorf("archive member mismatch: got %d expected %d", count, seg.Points)
	}
	footer := telemetryRecord{Kind: "footer", Count: count, Hash: hex.EncodeToString(plain.h.Sum(nil))}
	if err = json.NewEncoder(zw).Encode(footer); err != nil {
		return false, err
	}
	if err = zw.Close(); err != nil {
		return false, err
	}
	if err = t.fault("archive-before-sync"); err != nil {
		return false, err
	}
	if err = f.Sync(); err != nil {
		return false, err
	}
	if err = f.Close(); err != nil {
		return false, err
	}
	closed = true
	seg.Hash = hex.EncodeToString(compressed.h.Sum(nil))
	seg.FileBytes = compressed.n
	if err = t.visitArchivePath(ctx, seg, part, nil); err != nil {
		return false, err
	}
	if err = t.fault("archive-before-rename"); err != nil {
		return false, err
	}
	tx, err := t.begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = t.renewWork(ctx, tx, "archive"); err != nil {
		return false, err
	}
	if err = t.root.Rename(part, path); err != nil {
		return false, err
	}
	d, e := t.root.Open(dir)
	if e != nil {
		return false, e
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return false, err
	}
	if err = t.fault("archive-after-rename"); err != nil {
		return false, err
	}
	if err = t.fault("archive-before-catalog"); err != nil {
		return false, err
	}
	r, err := tx.ExecContext(ctx, t.bind("UPDATE "+t.table("segments")+" SET state='archived',checksum=?,file_bytes=? WHERE id=? AND state='writing'"), seg.Hash, seg.FileBytes, id)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, errors.New("archive publication ownership changed")
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, t.fault("archive-after-catalog")
}

func (t *Telemetry) visitArchive(ctx context.Context, seg telemetrySegment, visit func(telemetryRecord) error) error {
	return t.visitArchivePath(ctx, seg, t.segmentPath(seg), visit)
}
func (t *Telemetry) visitArchivePath(ctx context.Context, seg telemetrySegment, path string, visit func(telemetryRecord) error) error {
	info, err := t.root.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != seg.FileBytes {
		return errors.New("archive type/length mismatch")
	}
	f, err := t.root.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	compressedHash := sha256.New()
	compressed := io.TeeReader(f, compressedHash)
	zr, err := zstd.NewReader(compressed, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(16<<20), zstd.WithDecoderMaxWindow(8<<20))
	if err != nil {
		return err
	}
	defer zr.Close()
	// Bound the complete decoded stream as well as each JSONL record.
	scanner := bufio.NewScanner(io.LimitReader(zr, t.cfg.SegmentBytes*4+(1<<20)))
	scanner.Buffer(make([]byte, 32<<10), 64<<10)
	h := sha256.New()
	var count int64
	header, footer := false, false
	var after int64
	ordinal := -1
	for scanner.Scan() {
		if err = ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		var r telemetryRecord
		if err = json.Unmarshal(line, &r); err != nil {
			return err
		}
		if footer {
			return errors.New("archive trailing records")
		}
		switch r.Kind {
		case "header":
			if header || count != 0 || r.Version != 1 || r.Segment != seg.ID {
				return errors.New("archive header/schema mismatch")
			}
			header = true
		case "point":
			if !header || r.Point == nil || r.Ingest < seg.First || r.Ingest > seg.Last || r.Ordinal < 0 || r.Ingest < after || (r.Ingest == after && r.Ordinal <= ordinal) {
				return errors.New("archive membership/order mismatch")
			}
			if err = r.Point.Validate(); err != nil {
				return err
			}
			if len(r.Point.Tags) > 32 || len(r.Point.Labels) > 32 {
				return errors.New("archive label budget")
			}
			after = r.Ingest
			ordinal = r.Ordinal
			count++
			if count%128 == 0 {
				if err = telemetryYield(ctx); err != nil {
					return err
				}
			}
			if visit != nil {
				if err = visit(r); err != nil {
					return err
				}
			}
		case "footer":
			if !header || r.Count != count || count != seg.Points || r.Hash != hex.EncodeToString(h.Sum(nil)) {
				return errors.New("archive content/count checksum mismatch")
			}
			footer = true
			continue
		default:
			return errors.New("unknown archive record")
		}
		h.Write(line)
		h.Write([]byte{'\n'})
	}
	if err = scanner.Err(); err != nil {
		return err
	}
	if !footer {
		return errors.New("incomplete archive")
	}
	if hex.EncodeToString(compressedHash.Sum(nil)) != seg.Hash {
		return errors.New("archive compressed checksum mismatch")
	}
	return nil
}

// DeleteArchivedChunk only deletes rows with durable archive ownership,
// complete summary coverage and acknowledged traffic handoff. No legacy block
// table is involved. Cancellation rolls back just this small transaction.
func (t *Telemetry) DeleteArchivedChunk(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 || limit > 512 {
		return 0, fmt.Errorf("%w: deletion chunk", ErrInvalidArgument)
	}
	release, err := t.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { release() }()
	// Full compressed-file verification precedes the first destructive chunk,
	// outside a write transaction. Subsequent chunks check the immutable file's
	// identity/size; they never decompress or rehash it for every 512 rows.
	coverage, err := t.progress(ctx, t.s.reader(), "summary")
	if err != nil {
		return 0, err
	}
	var candidate, stateBefore string
	eligible := " FROM " + t.table("segments") + " a JOIN " + t.table("receipts") + " r ON r.ingest_id=a.last_id WHERE a.state IN ('archived','deleting') AND a.last_id<=? AND r.received<=? AND NOT EXISTS (SELECT 1 FROM " + t.table("outbox") + " o WHERE o.ingest_id>=a.first_id AND o.ingest_id<=a.last_id AND o.delivered=0) AND NOT EXISTS (SELECT 1 FROM " + t.table("leases") + " l WHERE l.segment_id=a.id AND l.expires>" + fmt.Sprintf("%d", time.Now().UnixNano()) + ")"
	err = t.s.reader().QueryRowContext(ctx, t.bind("SELECT a.id,a.state"+eligible+" ORDER BY a.created,a.id LIMIT 1"), coverage, now.Add(-t.cfg.RawWindow).UnixNano()).Scan(&candidate, &stateBefore)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if stateBefore == "archived" {
		seg, e := t.segment(ctx, t.s.reader(), candidate)
		if e != nil {
			return 0, e
		}
		// Immutable-file verification may yield for CPU pacing. Ingestion must
		// not wait behind decompression or those pauses. Eligibility is checked
		// again in the destructive transaction after reacquiring the writer.
		release()
		release = func() {}
		if e = t.visitArchive(ctx, seg, nil); e != nil {
			return 0, e
		}
		release, err = t.acquire(ctx)
		if err != nil {
			release = func() {}
			return 0, err
		}
	}
	tx, err := t.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	coverage, err = t.progress(ctx, tx, "summary")
	if err != nil {
		return 0, err
	}
	var id string
	err = tx.QueryRowContext(ctx, t.bind("SELECT a.id"+eligible+" AND a.id=?"), coverage, now.Add(-t.cfg.RawWindow).UnixNano(), candidate).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	seg, err := t.segment(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	// Check file identity/size at every short chunk; the full checksum was
	// verified before publication. A missing/truncated file fails closed.
	info, err := t.root.Lstat(t.segmentPath(seg))
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() != seg.FileBytes {
		return 0, errors.New("archive unavailable; retaining hot data")
	}
	rows, err := tx.QueryContext(ctx, t.bind("SELECT ingest_id,ordinal FROM "+t.table("hot")+" WHERE segment_id=? ORDER BY ingest_id,ordinal LIMIT ?"), id, limit)
	if err != nil {
		return 0, err
	}
	type key struct {
		i int64
		o int
	}
	var keys []key
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.i, &k.o); err != nil {
			break
		}
		keys = append(keys, k)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if readErr != nil {
		return 0, readErr
	}
	for _, k := range keys {
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("hot")+" WHERE ingest_id=? AND ordinal=? AND segment_id=?"), k.i, k.o, id); err != nil {
			return 0, err
		}
	}
	state := "deleting"
	if int64(len(keys))+seg.Deleted == seg.Points {
		state = "complete"
		hot, err := t.progress(ctx, tx, "hot_bytes")
		if err != nil {
			return 0, err
		}
		if err = t.advance(ctx, tx, "hot_bytes", hot, max(0, hot-seg.Bytes)); err != nil {
			return 0, err
		}
	}
	if len(keys) == 0 && state != "complete" {
		return 0, errors.New("archive deletion progress mismatch")
	}
	if _, err = tx.ExecContext(ctx, t.bind("UPDATE "+t.table("segments")+" SET state=?,deleted=deleted+? WHERE id=?"), state, len(keys), id); err != nil {
		return 0, err
	}
	if err = t.fault("delete-before-commit"); err != nil {
		return 0, err
	}
	return len(keys), tx.Commit()
}

type TelemetryTrafficEvent struct {
	StoreID  string
	IngestID int64
	Source   string
	Received time.Time
	Observed time.Time
	Traffic  TelemetryTraffic
}

// DeliverTraffic calls an idempotent metadata transaction before marking an
// outbox row. A crash between those commits intentionally causes redelivery.
func (t *Telemetry) DeliverTraffic(ctx context.Context, limit int, apply func(context.Context, TelemetryTrafficEvent) error) (int, error) {
	if limit <= 0 || limit > 256 || apply == nil {
		return 0, ErrInvalidArgument
	}
	select {
	case t.trafficGate <- struct{}{}:
		defer func() { <-t.trafficGate }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	if err := t.claimWork(ctx, "traffic"); err != nil {
		return 0, err
	}
	defer t.releaseWork("traffic")
	rows, err := t.s.reader().QueryContext(ctx, t.bind("SELECT o.ingest_id,o.source,o.received,o.payload,r.observed FROM "+t.table("outbox")+" o JOIN "+t.table("receipts")+" r ON r.ingest_id=o.ingest_id WHERE o.delivered=0 ORDER BY o.ingest_id LIMIT ?"), limit)
	if err != nil {
		return 0, err
	}
	var events []TelemetryTrafficEvent
	for rows.Next() {
		var e TelemetryTrafficEvent
		var ns int64
		var observed int64
		var raw string
		if err = rows.Scan(&e.IngestID, &e.Source, &ns, &raw, &observed); err != nil {
			break
		}
		e.Received = time.Unix(0, ns).UTC()
		e.StoreID = t.instance
		e.Observed = time.Unix(0, observed).UTC()
		if err = json.Unmarshal([]byte(raw), &e.Traffic); err != nil {
			break
		}
		if e.Traffic.ObservedAt != 0 {
			e.Observed = time.Unix(0, e.Traffic.ObservedAt).UTC()
		}
		events = append(events, e)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if readErr != nil {
		return 0, readErr
	}
	for i, e := range events {
		if err = t.renewWork(ctx, t.s.db, "traffic"); err != nil {
			return i, err
		}
		if err = apply(ctx, e); err != nil {
			return i, err
		}
		if err = t.fault("outbox-after-consumer"); err != nil {
			return i, err
		}
		if _, err = t.s.db.ExecContext(ctx, t.bind("UPDATE "+t.table("outbox")+" SET delivered=1 WHERE ingest_id=? AND delivered=0"), e.IngestID); err != nil {
			return i, err
		}
	}
	return len(events), nil
}

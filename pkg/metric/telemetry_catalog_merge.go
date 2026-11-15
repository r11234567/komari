package metric

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Handoff extents bound the writer queue; they are not permanent catalog
// partitions. Once all SQL handoff references retire, join adjacent extents
// in place. Their compressed bytes and embedded indexes remain untouched.
// Leases exclude both owners so frozen exports/backups keep stable ownership.
func (t *Telemetry) mergeCatalogPair(ctx context.Context, tx *sql.Tx, now time.Time, limit int) (int, error) {
	eligible := func(alias string) string {
		return alias + ".state='complete' AND " + alias + ".checksum='' AND " + alias + ".archive_file<>''" +
			" AND NOT EXISTS (SELECT 1 FROM " + t.table("receipts") + " r WHERE r.segment_id=" + alias + ".id)" +
			" AND NOT EXISTS (SELECT 1 FROM " + t.table("hot") + " h WHERE h.segment_id=" + alias + ".id)" +
			" AND NOT EXISTS (SELECT 1 FROM " + t.table("summary") + " s WHERE s.segment_id=" + alias + ".id)" +
			" AND NOT EXISTS (SELECT 1 FROM " + t.table("gaps") + " g WHERE g.segment_id=" + alias + ".id)" +
			" AND NOT EXISTS (SELECT 1 FROM " + t.table("leases") + " l WHERE l.segment_id=" + alias + ".id AND l.expires>?)"
	}
	var left, right string
	err := tx.QueryRowContext(ctx, t.bind("SELECT a.id,b.id FROM "+t.table("segments")+" a JOIN "+t.table("segments")+" b ON b.archive_file=a.archive_file AND b.archive_offset=a.archive_offset+a.file_bytes AND b.archive_entity=a.archive_entity AND b.archive_day=a.archive_day WHERE "+eligible("a")+" AND "+eligible("b")+" AND a.archive_frames+b.archive_frames<=16384 AND a.archive_parts+b.archive_parts<=4096 AND a.archive_index_bytes+b.archive_index_bytes<=8388608 AND (SELECT COUNT(*) FROM "+t.table("members")+" m WHERE m.segment_id=b.id)<=? ORDER BY a.archive_file,a.archive_offset LIMIT 1"), now.UnixNano(), now.UnixNano(), limit).Scan(&left, &right)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	a, err := t.segment(ctx, tx, left)
	if err != nil {
		return 0, err
	}
	b, err := t.segment(ctx, tx, right)
	if err != nil {
		return 0, err
	}
	// One bounded index read per pre-v11 extent seeds reader limits. New
	// archives publish these counters directly; merging never rescans a day.
	for _, s := range []telemetrySegment{a, b} {
		if s.ArchiveParts > 0 {
			continue
		}
		f, e := t.root.Open(t.segmentPath(s))
		if e != nil {
			return 0, e
		}
		index, e := readTelemetryArchiveIndex(telemetryArchiveReader(f, s), s)
		f.Close()
		if e != nil {
			return 0, e
		}
		if index == nil || index.Version != 3 {
			return 0, errors.New("catalog merge requires a v3 archive")
		}
		var dataBytes int64
		for _, frame := range index.Frames {
			dataBytes += frame.Bytes
		}
		_, e = tx.ExecContext(ctx, t.bind("UPDATE "+t.table("segments")+" SET archive_frames=?,archive_parts=1,archive_index_bytes=? WHERE id=?"), len(index.Frames), s.FileBytes-dataBytes, s.ID)
		return 1, e
	}
	rows, err := tx.QueryContext(ctx, t.bind("SELECT series_id,first_observed,last_observed,points FROM "+t.table("members")+" WHERE segment_id=?"), right)
	if err != nil {
		return 0, err
	}
	type member struct {
		id                  string
		first, last, points int64
	}
	var members []member
	for rows.Next() {
		var m member
		if err = rows.Scan(&m.id, &m.first, &m.last, &m.points); err != nil {
			break
		}
		members = append(members, m)
	}
	re := rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if re != nil {
		return 0, re
	}
	for _, m := range members {
		var first, last, points int64
		err = tx.QueryRowContext(ctx, t.bind("SELECT first_observed,last_observed,points FROM "+t.table("members")+" WHERE segment_id=? AND series_id=?"), left, m.id).Scan(&first, &last, &points)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("members")+" (segment_id,series_id,first_observed,last_observed,points) VALUES (?,?,?,?,?)"), left, m.id, m.first, m.last, m.points)
		} else if err == nil {
			_, err = tx.ExecContext(ctx, t.bind("UPDATE "+t.table("members")+" SET first_observed=?,last_observed=?,points=? WHERE segment_id=? AND series_id=?"), min(first, m.first), max(last, m.last), points+m.points, left, m.id)
		}
		if err != nil {
			return 0, err
		}
	}
	if _, err = tx.ExecContext(ctx, t.bind("UPDATE "+t.table("segments")+" SET first_id=?,last_id=?,points=?,bytes=?,created=?,deleted=?,file_bytes=?,archive_frames=?,archive_parts=?,archive_index_bytes=? WHERE id=?"), min(a.First, b.First), max(a.Last, b.Last), a.Points+b.Points, a.Bytes+b.Bytes, max(a.Created, b.Created), a.Deleted+b.Deleted, a.FileBytes+b.FileBytes, a.ArchiveFrames+b.ArchiveFrames, a.ArchiveParts+b.ArchiveParts, a.ArchiveIndexBytes+b.ArchiveIndexBytes, left); err != nil {
		return 0, err
	}
	for _, table := range []string{"members", "leases"} {
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table(table)+" WHERE segment_id=?"), right); err != nil {
			return 0, err
		}
	}
	if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("segments")+" WHERE id=?"), right); err != nil {
		return 0, err
	}
	if err = t.fault("catalog-merge-before-commit"); err != nil {
		return 0, err
	}
	return max(1, len(members)), nil
}

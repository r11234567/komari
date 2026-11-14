package metric

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

const telemetryArchiveFileLimit = 64 << 20

// Logical handoff units are immutable extents of a machine/day file. Appending
// copies compressed bytes; it never decodes or rewrites a published prefix.
// SQL owns the committed end. Recovery truncates only an unregistered tail.
func (t *Telemetry) appendArchive(ctx context.Context, seg telemetrySegment, part string) (telemetrySegment, error) {
	seg.ArchiveFile = seg.ID
	if seg.ArchiveEntity != "" {
		var file string
		err := t.s.reader().QueryRowContext(ctx, t.bind("SELECT archive_file FROM "+t.table("segments")+" WHERE archive_entity=? AND archive_day=? AND archive_file<>'' AND state IN ('archived','deleting','complete') ORDER BY last_id DESC LIMIT 1"), seg.ArchiveEntity, seg.ArchiveDay).Scan(&file)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return seg, err
		}
		if err == nil {
			seg.ArchiveFile = file
		}
	}
	var prefix int64
	if err := t.s.reader().QueryRowContext(ctx, t.bind("SELECT COALESCE(MAX(archive_offset+file_bytes),0) FROM "+t.table("segments")+" WHERE archive_file=? AND state IN ('archived','deleting','complete')"), seg.ArchiveFile).Scan(&prefix); err != nil {
		return seg, err
	}
	if prefix+seg.FileBytes > telemetryArchiveFileLimit {
		seg.ArchiveFile = seg.ID
		prefix = 0
	}
	seg.ArchiveOffset = prefix
	path := t.segmentPath(seg)
	info, err := t.root.Lstat(path)
	created := errors.Is(err, os.ErrNotExist)
	if err != nil && !created {
		return seg, err
	}
	if !created && (!info.Mode().IsRegular() || info.Size() < prefix) {
		return seg, errors.New("archive published prefix unavailable")
	}
	if created && prefix != 0 {
		return seg, errors.New("archive published file missing")
	}
	out, err := t.root.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return seg, err
	}
	defer out.Close()
	if err = out.Truncate(prefix); err != nil {
		return seg, err
	}
	if _, err = out.Seek(prefix, io.SeekStart); err != nil {
		return seg, err
	}
	in, err := t.root.Open(part)
	if err != nil {
		return seg, err
	}
	defer in.Close()
	var n int64
	buf := make([]byte, 64<<10)
	renewed := time.Now()
	for {
		if err = ctx.Err(); err != nil {
			return seg, err
		}
		if time.Since(renewed) >= t.cfg.WorkLease/3 {
			if err = t.renewWork(ctx, t.s.db, "archive"); err != nil {
				return seg, err
			}
			renewed = time.Now()
		}
		r, e := in.Read(buf)
		if r > 0 {
			written, writeErr := out.Write(buf[:r])
			n += int64(written)
			if writeErr != nil {
				return seg, writeErr
			}
			if written != r {
				return seg, io.ErrShortWrite
			}
		}
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return seg, e
		}
	}
	if n != seg.FileBytes {
		return seg, io.ErrUnexpectedEOF
	}
	if err = out.Sync(); err != nil {
		return seg, err
	}
	if created {
		d, e := t.root.Open(filepath.Dir(path))
		if e != nil {
			return seg, e
		}
		e = d.Sync()
		d.Close()
		if e != nil {
			return seg, e
		}
	}
	return seg, nil
}

func telemetryArchiveExtentValid(info os.FileInfo, seg telemetrySegment) bool {
	if !info.Mode().IsRegular() || seg.ArchiveOffset < 0 || seg.FileBytes < 0 {
		return false
	}
	if seg.ArchiveFile == "" {
		return info.Size() == seg.FileBytes
	}
	return seg.ArchiveOffset <= info.Size() && seg.FileBytes <= info.Size()-seg.ArchiveOffset
}

func telemetryArchiveReader(f *os.File, seg telemetrySegment) *io.SectionReader {
	return io.NewSectionReader(f, seg.ArchiveOffset, seg.FileBytes)
}

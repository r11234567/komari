package metric

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RollbackLegacy prepares a separate SQL file for the previous reader while
// the service is stopped. Original legacy summaries survive; acknowledged new
// hot/cold samples are replayed as ordinary rows. It never rewinds the source,
// deletes its archives, or changes the independent metadata/calibration ledger.
// Unavailable raw is a hard failure, not a successful partial rollback.
func (t *Telemetry) RollbackLegacy(ctx context.Context, destination string, observe func(int64)) error {
	if t.s.cfg.Driver != DriverSQLite {
		return fmt.Errorf("offline rollback currently requires SQLite")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return fmt.Errorf("rollback destination must not exist")
	}
	dir, err := os.MkdirTemp(filepath.Dir(destination), ".telemetry-rollback-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "metrics.db")
	if err = t.s.BackupSQLite(ctx, path); err != nil {
		return err
	}
	cfg := SQLite(path, WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable))
	cfg.TablePrefix = t.s.cfg.TablePrefix
	target, err := Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer target.Close()
	// Delete source-era rows hidden by an administrator before replaying later
	// accepted samples. This also prevents old rollups resurrecting erased data.
	erased := map[string]int64{}
	after := ""
	for {
		rows, err := t.s.reader().QueryContext(ctx, t.bind("SELECT s.id,s.metric,s.entity,s.tags,e.through_id FROM "+t.table("erasures")+" e JOIN "+t.table("series")+" s ON s.id=e.series_id WHERE s.id>? ORDER BY s.id LIMIT 128"), after)
		if err != nil {
			return err
		}
		type item struct {
			id      string
			q       Query
			through int64
		}
		var items []item
		for rows.Next() {
			var v item
			var tags string
			if err = rows.Scan(&v.id, &v.q.MetricName, &v.q.EntityID, &tags, &v.through); err != nil {
				break
			}
			if err = json.Unmarshal([]byte(tags), &v.q.Tags); err != nil {
				break
			}
			items = append(items, v)
		}
		re := rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if re != nil {
			return re
		}
		if len(items) == 0 {
			break
		}
		for _, v := range items {
			if len(erased) >= 16384 {
				return ErrTelemetryBackpressure
			}
			erased[v.id] = v.through
			if _, err = target.DeleteSeries(ctx, v.q); err != nil {
				return err
			}
			after = v.id
		}
	}
	var total int64
	points := make([]Point, 0, 256)
	flush := func() error {
		if len(points) == 0 {
			return nil
		}
		if err := target.WriteBatch(ctx, points); err != nil {
			return err
		}
		total += int64(len(points))
		points = points[:0]
		if observe != nil {
			observe(total)
		}
		return nil
	}
	visit := func(r telemetryRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.Point == nil {
			return nil
		}
		p := r.Point.normalized()
		tags, _ := json.Marshal(p.Tags)
		h := sha256.Sum256([]byte(p.MetricName + "\x00" + p.EntityID + "\x00" + string(tags)))
		if r.Ingest <= erased[hex.EncodeToString(h[:])] {
			return nil
		}
		points = append(points, p)
		if len(points) == 256 {
			return flush()
		}
		return nil
	}
	var previous int64
	lastID := ""
	for {
		var id string
		err = t.s.reader().QueryRowContext(ctx, t.bind("SELECT id FROM "+t.table("segments")+" WHERE state<>'legacy' AND (first_id>? OR (first_id=? AND id>?)) ORDER BY first_id,id LIMIT 1"), previous, previous, lastID).Scan(&id)
		if err == sql.ErrNoRows {
			break
		}
		if err != nil {
			return err
		}
		seg, err := t.segment(ctx, t.s.reader(), id)
		if err != nil {
			return err
		}
		switch seg.State {
		case "archived", "deleting", "complete":
			if err = t.visitArchive(ctx, seg, visit); err != nil {
				return err
			}
		case "open", "sealed", "writing":
			var ingest int64
			ordinal := -1
			for {
				page, err := t.segmentPage(ctx, id, ingest, ordinal, 128)
				if err != nil {
					return err
				}
				if len(page) == 0 {
					break
				}
				for _, r := range page {
					if err = visit(r); err != nil {
						return err
					}
					ingest, ordinal = r.Ingest, r.Ordinal
				}
			}
		default:
			return fmt.Errorf("%w: rollback raw unavailable for segment %s", ErrTelemetryCoverage, id)
		}
		previous, lastID = seg.First, seg.ID
	}
	if err = flush(); err != nil {
		return err
	}
	// Never let a future new-engine startup silently use the pre-replay target.
	for _, name := range []string{"gaps", "traffic_ingress", "identity", "erasures", "members", "work", "leases", "outbox", "summary", "hot", "series", "receipts", "segments", "progress"} {
		if _, err = target.db.ExecContext(ctx, "DROP TABLE "+t.table(name)); err != nil {
			return err
		}
	}
	if err = target.Close(); err != nil {
		return err
	}
	if err = os.Link(path, destination); err != nil {
		return err
	}
	file, err := os.Open(destination)
	if err != nil {
		return err
	}
	err = file.Sync()
	file.Close()
	if err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

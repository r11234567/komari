package metric

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LegacyPointImport is a stopped-service, ordered source stream. One small
// cursor per source/table replaces per-point import proofs. The source must
// remain unchanged while an interrupted import is resumed.
type LegacyPointImport struct {
	t                   *Telemetry
	id                  string
	position, committed int64
	now                 time.Time
}

type telemetryImportCheckpoint struct {
	id            string
	before, after int64
}
type telemetryImportCheckpointKey struct{}

func (p *LegacyPointImport) ValidateComplete() error {
	if p.position < p.committed {
		return fmt.Errorf("legacy import source shrank below its committed cursor; preserve the original source")
	}
	return nil
}

func (t *Telemetry) BeginLegacyPointImport(ctx context.Context, source, stream string) (*LegacyPointImport, error) {
	if source == "" || stream == "" || len(source) > 4096 || len(stream) > 64 {
		return nil, ErrInvalidArgument
	}
	release, err := t.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	// Optional offline metadata only: a handful of rows, no point identity
	// index, content hash, payload copy, or dependency on retired SQL tables.
	if _, err = t.s.db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+t.table("imports")+" (id CHAR(32) PRIMARY KEY,source TEXT NOT NULL,stream VARCHAR(64) NOT NULL,position BIGINT NOT NULL)"); err != nil {
		return nil, err
	}
	p := &LegacyPointImport{t: t, now: time.Now().UTC()}
	err = t.s.db.QueryRowContext(ctx, t.bind("SELECT id,position FROM "+t.table("imports")+" WHERE source=? AND stream=?"), source, stream).Scan(&p.id, &p.committed)
	if errors.Is(err, sql.ErrNoRows) {
		p.id = telemetryID()
		_, err = t.s.db.ExecContext(ctx, t.bind("INSERT INTO "+t.table("imports")+" (id,source,stream,position) VALUES (?,?,?,0)"), p.id, source, stream)
	}
	return p, err
}

func (t *Telemetry) importCheckpoint(ctx context.Context, tx *sql.Tx, c telemetryImportCheckpoint, advance bool) (bool, error) {
	var position int64
	if err := tx.QueryRowContext(ctx, t.bind("SELECT position FROM "+t.table("imports")+" WHERE id=?"), c.id).Scan(&position); err != nil {
		return false, err
	}
	if position >= c.after {
		return true, nil
	}
	if position != c.before {
		return false, fmt.Errorf("legacy import source cursor changed; resume from the unchanged source")
	}
	if advance {
		r, err := tx.ExecContext(ctx, t.bind("UPDATE "+t.table("imports")+" SET position=? WHERE id=? AND position=?"), c.after, c.id, c.before)
		if err != nil {
			return false, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return false, err
		}
		if n != 1 {
			return false, fmt.Errorf("legacy import cursor concurrently changed")
		}
	}
	return false, nil
}

// Write accepts source order, splits bounded machine/day batches, and commits
// each cursor with its accepted points. Replays skip committed input even after
// receipts and hot rows have been retired into archives.
func (p *LegacyPointImport) Write(ctx context.Context, points []Point) error {
	for len(points) > 0 {
		if p.position < p.committed {
			n := min(int64(len(points)), p.committed-p.position)
			p.position += n
			points = points[n:]
			continue
		}
		n := 1
		first := points[0]
		day := first.Timestamp.UTC().Truncate(24 * time.Hour)
		for n < len(points) && n < 256 && points[n].EntityID == first.EntityID && points[n].Timestamp.UTC().Truncate(24*time.Hour).Equal(day) {
			n++
		}
		checkpoint := telemetryImportCheckpoint{p.id, p.committed, p.position + int64(n)}
		chunk, err := p.t.s.filterDisabledMetricPoints(ctx, points[:n])
		if err != nil {
			return err
		}
		physical := make([]Point, 0, len(chunk))
		for _, point := range chunk {
			if point.MetricName != sqliteVirtualPingLossMetric {
				physical = append(physical, point)
			}
		}
		if len(physical) > 0 {
			importCtx := context.WithValue(ctx, telemetryImportCheckpointKey{}, checkpoint)
			_, err = p.t.accept(importCtx, TelemetryBatch{Source: first.EntityID, Identity: fmt.Sprintf("import/%s/%d", p.id, checkpoint.after), Points: physical}, p.now, true)
		} else {
			var tx *sql.Tx
			tx, err = p.t.begin(ctx)
			if err == nil {
				_, err = p.t.importCheckpoint(ctx, tx, checkpoint, true)
				if err == nil {
					err = tx.Commit()
				}
				tx.Rollback()
			}
		}
		if err != nil {
			return err
		}
		p.position, p.committed = checkpoint.after, checkpoint.after
		points = points[n:]
		if err = p.t.drainLegacyPointImport(ctx, p.now); err != nil {
			return err
		}
	}
	return nil
}

func (t *Telemetry) drainLegacyPointImport(ctx context.Context, now time.Time) error {
	for {
		n, err := t.SummarizeNext(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	if _, err := t.sealDue(ctx, now, false); err != nil {
		return err
	}
	for {
		ok, err := t.ArchiveNext(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
	}
	for {
		n, err := t.DeleteArchivedChunk(ctx, now, 512)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	for {
		n, err := t.PruneReceipts(ctx, now, 512)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

// FinishLegacyPointImport publishes no completion until handoff, age promotion
// and directory cleanup succeed. Stream cursors remain small and retryable.
func (t *Telemetry) FinishLegacyPointImport(ctx context.Context) error {
	if err := t.finishLegacyRawHandoff(ctx); err != nil {
		return err
	}
	now := time.Now().UTC()
	for {
		n, err := t.PromoteLegacyChunk(ctx, now, 512)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	for {
		n, err := t.PruneReceipts(ctx, now, 512)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	for {
		n, err := t.PruneCatalogChunk(ctx, now, 512)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

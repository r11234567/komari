package metric

// This reader is only used with the source stopped. Every query is keyset
// paged and closed before a target write: neither a source-size slice nor an
// OFFSET that repeatedly inflates all old blocks is needed. Traversal order is
// deterministic so a restarted window reuses the same durable batch receipts.
import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

func (s *Store) visitLegacyMigrationRaw(ctx context.Context, t *Telemetry, name, entity string, start, end time.Time, visit func([]Point) error) error {
	batch := make([]Point, 0, 256)
	emit := func(p Point) error {
		batch = append(batch, p)
		if len(batch) < 256 {
			return nil
		}
		if err := visit(batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	if s.sqliteStorageV4 || s.externalPointBlocks {
		var cursor int64
		for {
			var sid int64
			var rawTags any
			err := s.reader().QueryRowContext(ctx, t.bind("SELECT id,tags FROM "+s.tables.series+" WHERE metric_name=? AND entity_id=? AND id>? ORDER BY id LIMIT 1"), name, entity, cursor).Scan(&sid, &rawTags)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return err
			}
			tags, err := decodeMap(rawTags)
			if err != nil {
				return err
			}
			if err = s.visitLegacyMigrationBlocks(ctx, t, sid, name, entity, tags, start, end, emit); err != nil {
				return err
			}
			cursor = sid
		}
	}
	if !s.sqliteStorageV4 {
		// External drivers keep unsealed points in the ordinary legacy table.
		var cursor int64
		for {
			rows, err := s.reader().QueryContext(ctx, t.bind("SELECT id,ts_nano,value,tags,labels FROM "+s.tables.points+" WHERE metric_name=? AND entity_id=? AND ts_nano>=? AND ts_nano<=? AND id>? ORDER BY id LIMIT 256"), name, entity, start.UnixNano(), end.UnixNano(), cursor)
			if err != nil {
				return err
			}
			var points []Point
			for rows.Next() {
				var ts int64
				var rawTags, rawLabels any
				p := Point{MetricName: name, EntityID: entity}
				if err = rows.Scan(&cursor, &ts, &p.Value, &rawTags, &rawLabels); err != nil {
					break
				}
				p.Timestamp = time.Unix(0, ts).UTC()
				p.Tags, err = decodeMap(rawTags)
				if err == nil {
					p.Labels, err = decodeMap(rawLabels)
				}
				if err != nil {
					break
				}
				points = append(points, p)
			}
			re := rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if re != nil {
				return re
			}
			for _, p := range points {
				if err = emit(p); err != nil {
					return err
				}
			}
			if len(points) < 256 {
				break
			}
		}
	}
	if len(batch) > 0 {
		return visit(batch)
	}
	return nil
}

func (s *Store) visitLegacyMigrationBlocks(ctx context.Context, t *Telemetry, sid int64, name, entity string, tags map[string]string, start, end time.Time, emit func(Point) error) error {
	hotWhere := "series_id=?"
	hotArgs := []any{sid}
	hotTable := s.tables.pointValues
	if !s.sqliteStorageV4 {
		var hash string
		if err := s.reader().QueryRowContext(ctx, t.bind("SELECT tags_hash FROM "+s.tables.series+" WHERE id=?"), sid).Scan(&hash); err != nil {
			return err
		}
		hotWhere = "metric_name=? AND entity_id=? AND tags_hash=?"
		hotArgs = []any{name, entity, hash}
		hotTable = s.tables.points
	}
	blockCursor := int64(math.MinInt64)
	previousEnd := int64(math.MinInt64)
	for {
		q := "SELECT b.start_nano,b.end_nano,b.point_count,b.codec,b.checksum,b.payload"
		if s.sqliteStorageV4 {
			q += ",b.axis_id,a.codec,a.checksum,a.payload"
		}
		q += " FROM " + s.tables.pointBlocks + " b"
		if s.sqliteStorageV4 {
			q += " LEFT JOIN " + s.tables.pointAxes + " a ON a.id=b.axis_id"
		}
		q += " WHERE b.series_id=? AND b.end_nano>=? AND b.start_nano<=? AND b.start_nano>? ORDER BY b.start_nano LIMIT 1"
		var from, to, checksum int64
		var count, codec int
		var payload, axisPayload []byte
		var axisID, axisCodec, axisChecksum sql.NullInt64
		args := []any{&from, &to, &count, &codec, &checksum, &payload}
		if s.sqliteStorageV4 {
			args = append(args, &axisID, &axisCodec, &axisChecksum, &axisPayload)
		}
		err := s.reader().QueryRowContext(ctx, t.bind(q), sid, start.UnixNano(), end.UnixNano(), blockCursor).Scan(args...)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		if from <= previousEnd {
			return fmt.Errorf("%w: overlapping legacy raw blocks; source retained", ErrTelemetryCoverage)
		}
		var points []sqliteV4BlockPoint
		if s.sqliteStorageV4 {
			points, err = decodeSQLiteStoredPointBlock(codec, count, uint32(checksum), payload, int(axisCodec.Int64), uint32(axisChecksum.Int64), axisPayload)
		} else {
			points, err = decodeSQLiteV4Block(codec, count, uint32(checksum), payload)
		}
		if err != nil {
			return err
		}
		if len(points) == 0 || points[0].timestamp != from || points[len(points)-1].timestamp != to {
			return fmt.Errorf("%w: legacy raw block boundary mismatch", ErrTelemetryCoverage)
		}
		// Hot values override sealed values at the same timestamp. The set is
		// bounded by this decoded block, even if the hot range is very large.
		keep := make(map[int64]bool, len(points))
		for _, p := range points {
			keep[p.timestamp] = true
		}
		hargs := append(append([]any{}, hotArgs...), max(from, start.UnixNano()), min(to, end.UnixNano()))
		rows, err := s.reader().QueryContext(ctx, t.bind("SELECT ts_nano FROM "+hotTable+" WHERE "+hotWhere+" AND ts_nano>=? AND ts_nano<=?"), hargs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ts int64
			if err = rows.Scan(&ts); err != nil {
				break
			}
			delete(keep, ts)
		}
		re := rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if re != nil {
			return re
		}
		for _, p := range points {
			if !keep[p.timestamp] || p.timestamp < start.UnixNano() || p.timestamp > end.UnixNano() {
				continue
			}
			labels, err := decodeMapString(p.labels)
			if err != nil {
				return err
			}
			if err = emit(Point{MetricName: name, EntityID: entity, Timestamp: time.Unix(0, p.timestamp).UTC(), Value: math.Float64frombits(p.valueBits), Tags: tags, Labels: labels}); err != nil {
				return err
			}
		}
		blockCursor = from
		previousEnd = to
	}
	if !s.sqliteStorageV4 {
		return nil
	}
	for cursor := start.UnixNano() - 1; ; {
		args := append(append([]any{}, hotArgs...), cursor, end.UnixNano())
		rows, err := s.reader().QueryContext(ctx, t.bind("SELECT ts_nano,value,labels FROM "+hotTable+" WHERE "+hotWhere+" AND ts_nano>? AND ts_nano<=? ORDER BY ts_nano LIMIT 256"), args...)
		if err != nil {
			return err
		}
		var points []Point
		for rows.Next() {
			var labels any
			p := Point{MetricName: name, EntityID: entity, Tags: tags}
			if err = rows.Scan(&cursor, &p.Value, &labels); err != nil {
				break
			}
			p.Timestamp = time.Unix(0, cursor).UTC()
			p.Labels, err = decodeMap(labels)
			if err != nil {
				break
			}
			points = append(points, p)
		}
		re := rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if re != nil {
			return re
		}
		for _, p := range points {
			if err = emit(p); err != nil {
				return err
			}
		}
		if len(points) < 256 {
			return nil
		}
	}
}

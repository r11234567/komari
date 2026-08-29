package metric

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const externalSealPointLimit = sqliteV4BlockPointLimit * 16

type externalPointSeries struct {
	id         int64
	metricName string
	entityID   string
	tagsHash   string
	tagsJSON   string
	tags       map[string]string
}

type externalStoredPoint struct {
	series externalPointSeries
	point  sqliteV4BlockPoint
}

func externalPointKey(metricName, entityID, tagsHash string, timestamp int64) string {
	return metricName + "\x00" + entityID + "\x00" + tagsHash + "\x00" + fmt.Sprint(timestamp)
}

func (s *Store) migrateExternalPointBlocks(ctx context.Context) error {
	if s.cfg.Driver == DriverSQLite {
		return nil
	}
	jsonType := s.dialect.jsonType()
	pk := s.dialect.autoIncrementPrimaryKey()
	blob := s.dialect.blobType()
	seriesSQL := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		id %s,
		metric_name VARCHAR(191) NOT NULL,
		entity_id VARCHAR(191) NOT NULL,
		tags_hash VARCHAR(64) NOT NULL,
		tags %s NOT NULL,
		UNIQUE(metric_name, entity_id, tags_hash)
	)`, s.tables.series, pk, jsonType)
	blocksSQL := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		series_id BIGINT NOT NULL,
		start_nano BIGINT NOT NULL,
		end_nano BIGINT NOT NULL,
		point_count INTEGER NOT NULL,
		codec INTEGER NOT NULL,
		checksum BIGINT NOT NULL,
		payload %s NOT NULL,
		PRIMARY KEY(series_id, start_nano)
	)`, s.tables.pointBlocks, blob)
	if s.cfg.Driver == DriverMySQL {
		seriesSQL += ` ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
		blocksSQL += ` ENGINE=InnoDB`
	}
	statements := []string{
		seriesSQL,
		blocksSQL,
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_external_series_lookup_idx ON %s (metric_name, entity_id)`, s.cfg.TablePrefix, s.tables.series),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_external_blocks_time_idx ON %s (start_nano, end_nano)`, s.cfg.TablePrefix, s.tables.pointBlocks),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_external_blocks_expiry_idx ON %s (series_id, end_nano)`, s.cfg.TablePrefix, s.tables.pointBlocks),
	}
	// MySQL cannot use CREATE INDEX IF NOT EXISTS. The unique key and primary
	// key cover point lookups; optional secondary indexes are omitted there.
	if s.cfg.Driver == DriverMySQL {
		statements = statements[:2]
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("metric: create external point block storage: %w", err)
		}
	}
	s.externalPointBlocks = true
	return nil
}

func (s *Store) matchingExternalPointSeries(ctx context.Context, q querier, query Query) ([]externalPointSeries, error) {
	args := make([]any, 0)
	parts := make([]string, 0)
	if strings.TrimSpace(query.MetricName) != "" {
		args = append(args, query.MetricName)
		parts = append(parts, "metric_name = "+s.dialect.placeholder(len(args)))
	}
	if strings.TrimSpace(query.EntityID) != "" {
		args = append(args, query.EntityID)
		parts = append(parts, "entity_id = "+s.dialect.placeholder(len(args)))
	}
	for _, key := range sortedKeys(query.Tags) {
		args = append(args, query.Tags[key])
		parts = append(parts, s.dialect.jsonExtractEquals("tags", key, s.dialect.placeholder(len(args))))
	}
	where := "1 = 1"
	if len(parts) > 0 {
		where = strings.Join(parts, " AND ")
	}
	rows, err := q.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, metric_name, entity_id, tags_hash, tags FROM %s WHERE %s ORDER BY id`,
		s.tables.series, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []externalPointSeries
	for rows.Next() {
		var item externalPointSeries
		var rawTags any
		if err := rows.Scan(&item.id, &item.metricName, &item.entityID, &item.tagsHash, &rawTags); err != nil {
			return nil, err
		}
		item.tagsJSON, err = rawTagsToJSON(rawTags)
		if err != nil {
			return nil, err
		}
		item.tags, err = decodeMapString(item.tagsJSON)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) queryExternalPointBlocks(ctx context.Context, q querier, query Query) ([]Point, error) {
	query = query.normalized()
	series, err := s.matchingExternalPointSeries(ctx, q, query)
	if err != nil {
		return nil, err
	}
	stored := make(map[string]externalStoredPoint)
	for _, item := range series {
		rows, err := q.QueryContext(ctx, fmt.Sprintf(
			`SELECT start_nano, end_nano, point_count, codec, checksum, payload
			 FROM %s WHERE series_id = %s AND end_nano >= %s AND start_nano <= %s`,
			s.tables.pointBlocks, s.dialect.placeholder(1), s.dialect.placeholder(2), s.dialect.placeholder(3)),
			item.id, query.Start.UnixNano(), query.End.UnixNano())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var startNano, endNano, checksum int64
			var count, codec int
			var payload []byte
			if err := rows.Scan(&startNano, &endNano, &count, &codec, &checksum, &payload); err != nil {
				_ = rows.Close()
				return nil, err
			}
			points, err := decodeSQLiteV4Block(codec, count, uint32(checksum), payload)
			if err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("metric: decode external point block series=%d start=%d: %w", item.id, startNano, err)
			}
			if len(points) == 0 || points[0].timestamp != startNano || points[len(points)-1].timestamp != endNano {
				_ = rows.Close()
				return nil, fmt.Errorf("metric: external point block boundary mismatch series=%d start=%d", item.id, startNano)
			}
			for _, point := range points {
				if point.timestamp < query.Start.UnixNano() || point.timestamp > query.End.UnixNano() {
					continue
				}
				key := externalPointKey(item.metricName, item.entityID, item.tagsHash, point.timestamp)
				stored[key] = externalStoredPoint{series: item, point: point}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}

	where, args := s.buildWhere(query)
	hotRows, err := q.QueryContext(ctx, fmt.Sprintf(
		`SELECT metric_name, entity_id, tags_hash, ts_nano, value, tags, labels, created_at FROM %s WHERE %s`,
		s.tables.points, where), args...)
	if err != nil {
		return nil, err
	}
	for hotRows.Next() {
		var item externalPointSeries
		var block sqliteV4BlockPoint
		var value float64
		var rawTags, rawLabels any
		if err := hotRows.Scan(&item.metricName, &item.entityID, &item.tagsHash, &block.timestamp, &value, &rawTags, &rawLabels, &block.createdAt); err != nil {
			_ = hotRows.Close()
			return nil, err
		}
		item.tagsJSON, err = rawTagsToJSON(rawTags)
		if err != nil {
			_ = hotRows.Close()
			return nil, err
		}
		item.tags, err = decodeMapString(item.tagsJSON)
		if err != nil {
			_ = hotRows.Close()
			return nil, err
		}
		block.labels, err = rawTagsToJSON(rawLabels)
		if err != nil {
			_ = hotRows.Close()
			return nil, err
		}
		block.valueBits = math.Float64bits(value)
		key := externalPointKey(item.metricName, item.entityID, item.tagsHash, block.timestamp)
		stored[key] = externalStoredPoint{series: item, point: block}
	}
	if err := hotRows.Err(); err != nil {
		_ = hotRows.Close()
		return nil, err
	}
	if err := hotRows.Close(); err != nil {
		return nil, err
	}

	result := make([]Point, 0, len(stored))
	for _, item := range stored {
		labels, err := decodeMapString(item.point.labels)
		if err != nil {
			return nil, err
		}
		result = append(result, Point{
			MetricName: item.series.metricName,
			EntityID:   item.series.entityID,
			Timestamp:  time.Unix(0, item.point.timestamp).UTC(),
			Value:      math.Float64frombits(item.point.valueBits),
			Tags:       cloneStringMap(item.series.tags),
			Labels:     labels,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Timestamp.Equal(result[j].Timestamp) {
			if result[i].MetricName != result[j].MetricName {
				return result[i].MetricName < result[j].MetricName
			}
			return result[i].EntityID < result[j].EntityID
		}
		if query.Order == OrderDesc {
			return result[i].Timestamp.After(result[j].Timestamp)
		}
		return result[i].Timestamp.Before(result[j].Timestamp)
	})
	start := query.Offset
	if start > len(result) {
		start = len(result)
	}
	end := len(result)
	if query.Limit > 0 && start+query.Limit < end {
		end = start + query.Limit
	}
	return result[start:end], nil
}

func (s *Store) externalSeriesID(ctx context.Context, tx *sql.Tx, item externalPointSeries) (int64, error) {
	args := []any{item.metricName, item.entityID, item.tagsHash, item.tagsJSON}
	var insert string
	switch s.cfg.Driver {
	case DriverPostgreSQL:
		insert = fmt.Sprintf(`INSERT INTO %s (metric_name, entity_id, tags_hash, tags)
			VALUES ($1, $2, $3, $4::jsonb) ON CONFLICT(metric_name, entity_id, tags_hash) DO NOTHING`, s.tables.series)
	case DriverMySQL:
		insert = fmt.Sprintf(`INSERT IGNORE INTO %s (metric_name, entity_id, tags_hash, tags) VALUES (?, ?, ?, ?)`, s.tables.series)
	default:
		return 0, fmt.Errorf("metric: external blocks unsupported for %s", s.cfg.Driver)
	}
	if _, err := tx.ExecContext(ctx, insert, args...); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT id FROM %s WHERE metric_name = %s AND entity_id = %s AND tags_hash = %s`,
		s.tables.series, s.dialect.placeholder(1), s.dialect.placeholder(2), s.dialect.placeholder(3)),
		item.metricName, item.entityID, item.tagsHash).Scan(&id)
	return id, err
}

func (s *Store) insertExternalBlock(ctx context.Context, tx *sql.Tx, seriesID int64, encoded sqliteV4EncodedBlock) error {
	var statement string
	switch s.cfg.Driver {
	case DriverPostgreSQL:
		statement = fmt.Sprintf(`INSERT INTO %s
			(series_id, start_nano, end_nano, point_count, codec, checksum, payload)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT(series_id, start_nano) DO UPDATE SET
			end_nano = EXCLUDED.end_nano, point_count = EXCLUDED.point_count,
			codec = EXCLUDED.codec, checksum = EXCLUDED.checksum, payload = EXCLUDED.payload`, s.tables.pointBlocks)
	case DriverMySQL:
		statement = fmt.Sprintf(`INSERT INTO %s
			(series_id, start_nano, end_nano, point_count, codec, checksum, payload)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE end_nano = VALUES(end_nano), point_count = VALUES(point_count),
			codec = VALUES(codec), checksum = VALUES(checksum), payload = VALUES(payload)`, s.tables.pointBlocks)
	}
	_, err := tx.ExecContext(ctx, statement, seriesID, encoded.startNano, encoded.endNano,
		encoded.count, encoded.codec, int64(encoded.checksum), encoded.payload)
	return err
}

func (s *Store) writeExternalBlocks(ctx context.Context, tx *sql.Tx, seriesID int64, points []sqliteV4BlockPoint) error {
	if len(points) == 0 {
		return nil
	}
	sort.Slice(points, func(i, j int) bool { return points[i].timestamp < points[j].timestamp })
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		`SELECT start_nano, point_count, codec, checksum, payload FROM %s
		 WHERE series_id = %s AND end_nano >= %s AND start_nano <= %s`,
		s.tables.pointBlocks, s.dialect.placeholder(1), s.dialect.placeholder(2), s.dialect.placeholder(3)),
		seriesID, points[0].timestamp, points[len(points)-1].timestamp)
	if err != nil {
		return err
	}
	byTimestamp := make(map[int64]sqliteV4BlockPoint, len(points))
	var overlappingStarts []int64
	for rows.Next() {
		var startNano, checksum int64
		var count, codec int
		var payload []byte
		if err := rows.Scan(&startNano, &count, &codec, &checksum, &payload); err != nil {
			_ = rows.Close()
			return err
		}
		decoded, err := decodeSQLiteV4Block(codec, count, uint32(checksum), payload)
		if err != nil {
			_ = rows.Close()
			return err
		}
		overlappingStarts = append(overlappingStarts, startNano)
		for _, point := range decoded {
			byTimestamp[point.timestamp] = point
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, point := range points {
		byTimestamp[point.timestamp] = point
	}
	points = points[:0]
	for _, point := range byTimestamp {
		points = append(points, point)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].timestamp < points[j].timestamp })
	for _, startNano := range overlappingStarts {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE series_id = %s AND start_nano = %s`,
			s.tables.pointBlocks, s.dialect.placeholder(1), s.dialect.placeholder(2)), seriesID, startNano); err != nil {
			return err
		}
	}
	for start := 0; start < len(points); start += sqliteV4BlockPointLimit {
		end := start + sqliteV4BlockPointLimit
		if end > len(points) {
			end = len(points)
		}
		encoded, err := encodeSQLiteV4Block(points[start:end])
		if err != nil {
			return err
		}
		decoded, err := decodeSQLiteV4Block(encoded.codec, encoded.count, encoded.checksum, encoded.payload)
		if err != nil || !sqliteV4PointsEqual(points[start:end], decoded) {
			if err == nil {
				err = errors.New("point block round-trip validation changed data")
			}
			return err
		}
		if err := s.insertExternalBlock(ctx, tx, seriesID, encoded); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) sealExternalPointBlocks(ctx context.Context, metricName string, before time.Time) (int64, error) {
	if !s.externalPointBlocks {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, s.dialect.compactTxOptions())
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT metric_name, entity_id, tags_hash, tags FROM %s
		 WHERE metric_name = %s AND ts_nano < %s ORDER BY ts_nano ASC LIMIT 1`,
		s.tables.points, s.dialect.placeholder(1), s.dialect.placeholder(2)), metricName, before.UTC().UnixNano())
	var item externalPointSeries
	var rawTags any
	if err := row.Scan(&item.metricName, &item.entityID, &item.tagsHash, &rawTags); errors.Is(err, sql.ErrNoRows) {
		return 0, tx.Commit()
	} else if err != nil {
		return 0, err
	}
	item.tagsJSON, err = rawTagsToJSON(rawTags)
	if err != nil {
		return 0, err
	}
	seriesID, err := s.externalSeriesID(ctx, tx, item)
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		`SELECT ts_nano, value, labels, created_at FROM %s
		 WHERE metric_name = %s AND entity_id = %s AND tags_hash = %s AND ts_nano < %s
		 ORDER BY ts_nano ASC LIMIT %d`, s.tables.points,
		s.dialect.placeholder(1), s.dialect.placeholder(2), s.dialect.placeholder(3), s.dialect.placeholder(4), externalSealPointLimit),
		item.metricName, item.entityID, item.tagsHash, before.UTC().UnixNano())
	if err != nil {
		return 0, err
	}
	var points []sqliteV4BlockPoint
	for rows.Next() {
		var point sqliteV4BlockPoint
		var value float64
		var labels any
		if err := rows.Scan(&point.timestamp, &value, &labels, &point.createdAt); err != nil {
			_ = rows.Close()
			return 0, err
		}
		point.valueBits = math.Float64bits(value)
		point.labels, err = rawTagsToJSON(labels)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(points) == 0 {
		return 0, tx.Commit()
	}
	startNano, endNano := points[0].timestamp, points[len(points)-1].timestamp
	if err := s.writeExternalBlocks(ctx, tx, seriesID, points); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE metric_name = %s AND entity_id = %s AND tags_hash = %s
		 AND ts_nano >= %s AND ts_nano <= %s`, s.tables.points,
		s.dialect.placeholder(1), s.dialect.placeholder(2), s.dialect.placeholder(3),
		s.dialect.placeholder(4), s.dialect.placeholder(5)),
		item.metricName, item.entityID, item.tagsHash, startNano, endNano)
	if err != nil {
		return 0, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if deleted != int64(len(points)) {
		return 0, fmt.Errorf("metric: external seal selected %d points but deleted %d", len(points), deleted)
	}
	return deleted, tx.Commit()
}

func (s *Store) deleteExternalPointsBeforeTx(ctx context.Context, tx *sql.Tx, metricName string, beforeNano int64) (int64, error) {
	series, err := s.matchingExternalPointSeries(ctx, tx, Query{MetricName: metricName})
	if err != nil {
		return 0, err
	}
	var deleted int64
	for _, item := range series {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT start_nano, point_count, codec, checksum, payload FROM %s
			 WHERE series_id = %s AND start_nano < %s`, s.tables.pointBlocks,
			s.dialect.placeholder(1), s.dialect.placeholder(2)), item.id, beforeNano)
		if err != nil {
			return deleted, err
		}
		var kept []sqliteV4BlockPoint
		var starts []int64
		for rows.Next() {
			var startNano, checksum int64
			var count, codec int
			var payload []byte
			if err := rows.Scan(&startNano, &count, &codec, &checksum, &payload); err != nil {
				_ = rows.Close()
				return deleted, err
			}
			points, err := decodeSQLiteV4Block(codec, count, uint32(checksum), payload)
			if err != nil {
				_ = rows.Close()
				return deleted, err
			}
			starts = append(starts, startNano)
			for _, point := range points {
				if point.timestamp < beforeNano {
					deleted++
				} else {
					kept = append(kept, point)
				}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return deleted, err
		}
		_ = rows.Close()
		for _, startNano := range starts {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE series_id = %s AND start_nano = %s`,
				s.tables.pointBlocks, s.dialect.placeholder(1), s.dialect.placeholder(2)), item.id, startNano); err != nil {
				return deleted, err
			}
		}
		if err := s.writeExternalBlocks(ctx, tx, item.id, kept); err != nil {
			return deleted, err
		}
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE metric_name = %s AND ts_nano < %s`,
		s.tables.points, s.dialect.placeholder(1), s.dialect.placeholder(2)), metricName, beforeNano)
	if err != nil {
		return deleted, err
	}
	hotDeleted, err := result.RowsAffected()
	return deleted + hotDeleted, err
}

// deleteExternalPointsBeforeBatchTx is the cooperative retention variant for
// external block stores. Complete blocks are removed by metadata only; at most
// one boundary block per series is decoded and rewritten, and hot rows are
// deleted with the same limit. This keeps a cleanup step bounded even when a
// series has months of history.
func (s *Store) deleteExternalPointsBeforeBatchTx(ctx context.Context, tx *sql.Tx, metricName string, beforeNano int64, limit int) (int64, bool, error) {
	series, err := s.matchingExternalPointSeries(ctx, tx, Query{MetricName: metricName})
	if err != nil {
		return 0, false, err
	}
	var deleted int64
	remaining := limit
	for _, item := range series {
		if limit > 0 && remaining <= 0 {
			break
		}
		limitSQL := ""
		if limit > 0 {
			limitSQL = fmt.Sprintf(" LIMIT %d", remaining)
		}
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT start_nano, point_count FROM %s
			 WHERE series_id = %s AND end_nano < %s ORDER BY start_nano%s`,
			s.tables.pointBlocks, s.dialect.placeholder(1), s.dialect.placeholder(2), limitSQL), item.id, beforeNano)
		if err != nil {
			return deleted, false, err
		}
		var blocks []struct{ start, count int64 }
		for rows.Next() {
			var block struct{ start, count int64 }
			if err := rows.Scan(&block.start, &block.count); err != nil {
				_ = rows.Close()
				return deleted, false, err
			}
			blocks = append(blocks, block)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return deleted, false, err
		}
		if err := rows.Close(); err != nil {
			return deleted, false, err
		}
		for _, block := range blocks {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE series_id = %s AND start_nano = %s`, s.tables.pointBlocks, s.dialect.placeholder(1), s.dialect.placeholder(2)), item.id, block.start); err != nil {
				return deleted, false, err
			}
			deleted += block.count
			if limit > 0 {
				remaining--
			}
		}
		if limit > 0 && remaining <= 0 {
			break
		}

		var startNano, endNano, checksum int64
		var count, codec int
		var payload []byte
		err = tx.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT start_nano, end_nano, point_count, codec, checksum, payload
			 FROM %s WHERE series_id = %s AND start_nano < %s AND end_nano >= %s
			 ORDER BY start_nano LIMIT 1`, s.tables.pointBlocks,
			s.dialect.placeholder(1), s.dialect.placeholder(2), s.dialect.placeholder(3)),
			item.id, beforeNano, beforeNano).Scan(&startNano, &endNano, &count, &codec, &checksum, &payload)
		if err == nil {
			points, decodeErr := decodeSQLiteV4Block(codec, count, uint32(checksum), payload)
			if decodeErr != nil {
				return deleted, false, decodeErr
			}
			kept := make([]sqliteV4BlockPoint, 0, len(points))
			for _, point := range points {
				if point.timestamp < beforeNano {
					deleted++
				} else {
					kept = append(kept, point)
				}
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE series_id = %s AND start_nano = %s`, s.tables.pointBlocks, s.dialect.placeholder(1), s.dialect.placeholder(2)), item.id, startNano); err != nil {
				return deleted, false, err
			}
			if err := s.writeExternalBlocks(ctx, tx, item.id, kept); err != nil {
				return deleted, false, err
			}
			if limit > 0 {
				remaining--
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return deleted, false, err
		}

		if limit > 0 && remaining <= 0 {
			break
		}
		hotLimit := remaining
		if hotLimit <= 0 {
			hotLimit = 1<<31 - 1
		}
		hotRows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE metric_name = %s AND ts_nano < %s ORDER BY ts_nano LIMIT %d`, s.tables.points, s.dialect.placeholder(1), s.dialect.placeholder(2), hotLimit), metricName, beforeNano)
		if err != nil {
			return deleted, false, err
		}
		var hotIDs []int64
		for hotRows.Next() {
			var id int64
			if err := hotRows.Scan(&id); err != nil {
				_ = hotRows.Close()
				return deleted, false, err
			}
			hotIDs = append(hotIDs, id)
		}
		if err := hotRows.Err(); err != nil {
			_ = hotRows.Close()
			return deleted, false, err
		}
		if err := hotRows.Close(); err != nil {
			return deleted, false, err
		}
		for _, id := range hotIDs {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = %s`, s.tables.points, s.dialect.placeholder(1)), id); err != nil {
				return deleted, false, err
			}
		}
		deleted += int64(len(hotIDs))
		if limit > 0 {
			remaining -= len(hotIDs)
		}
	}
	pending, err := queryRowExists(ctx, tx, fmt.Sprintf(
		`SELECT 1 FROM %s b JOIN %s s ON s.id = b.series_id WHERE s.metric_name = %s AND b.end_nano < %s LIMIT 1`,
		s.tables.pointBlocks, s.tables.series, s.dialect.placeholder(1), s.dialect.placeholder(2)), metricName, beforeNano)
	if err != nil || pending {
		return deleted, false, err
	}
	pending, err = queryRowExists(ctx, tx, fmt.Sprintf(
		`SELECT 1 FROM %s WHERE metric_name = %s AND ts_nano < %s LIMIT 1`,
		s.tables.points, s.dialect.placeholder(1), s.dialect.placeholder(2)), metricName, beforeNano)
	return deleted, !pending, err
}

func (s *Store) deleteExternalSeriesTx(ctx context.Context, tx *sql.Tx, filter Query) (int64, error) {
	if !s.externalPointBlocks {
		return 0, nil
	}
	series, err := s.matchingExternalPointSeries(ctx, tx, filter)
	if err != nil {
		return 0, err
	}
	var deleted int64
	for _, item := range series {
		var count sql.NullInt64
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT SUM(point_count) FROM %s WHERE series_id = %s`,
			s.tables.pointBlocks, s.dialect.placeholder(1)), item.id).Scan(&count); err != nil {
			return deleted, err
		}
		if count.Valid {
			deleted += count.Int64
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE series_id = %s`, s.tables.pointBlocks, s.dialect.placeholder(1)), item.id); err != nil {
			return deleted, err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE id = %s`, s.tables.series, s.dialect.placeholder(1)), item.id); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

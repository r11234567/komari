package metric

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// Statistics are mergeable deltas, not averages of averages. The sketch uses
// the versioned uncompressed TD encoding; hot updates never DEFLATE a blob.
type telemetryStats struct {
	N            int64   `json:"n"`
	Attempts     int64   `json:"attempts"`
	Loss         int64   `json:"loss"`
	Sum          float64 `json:"sum"`
	Mean         float64 `json:"mean"`
	M2           float64 `json:"m2"`
	Min          float64 `json:"min"`
	Max          float64 `json:"max"`
	First        float64 `json:"first"`
	Last         float64 `json:"last"`
	FirstNano    int64   `json:"first_ns"`
	LastNano     int64   `json:"last_ns"`
	FirstIngest  int64   `json:"first_ingest"`
	LastIngest   int64   `json:"last_ingest"`
	FirstOrdinal int     `json:"first_ordinal"`
	LastOrdinal  int     `json:"last_ordinal"`
	Digest       []byte  `json:"digest"`
	sketch       *TDigest
}

func (s *telemetryStats) add(p Point, ingest int64, ordinal int) {
	ns := p.Timestamp.UnixNano()
	if s.Attempts == 0 || ns < s.FirstNano || (ns == s.FirstNano && (ingest < s.FirstIngest || (ingest == s.FirstIngest && ordinal < s.FirstOrdinal))) {
		s.First = p.Value
		s.FirstNano = ns
		s.FirstIngest = ingest
		s.FirstOrdinal = ordinal
	}
	if s.Attempts == 0 || ns > s.LastNano || (ns == s.LastNano && (ingest > s.LastIngest || (ingest == s.LastIngest && ordinal > s.LastOrdinal))) {
		s.Last = p.Value
		s.LastNano = ns
		s.LastIngest = ingest
		s.LastOrdinal = ordinal
	}
	s.Attempts++
	if p.MetricName == sqliteMergedPingLatencyMetric && p.Value < 0 {
		s.Loss++
		return
	}
	s.N++
	if s.N == 1 {
		s.Min = p.Value
		s.Max = p.Value
	}
	s.Min = min(s.Min, p.Value)
	s.Max = max(s.Max, p.Value)
	s.Sum += p.Value
	delta := p.Value - s.Mean
	s.Mean += delta / float64(s.N)
	s.M2 += delta * (p.Value - s.Mean)
	if s.sketch == nil {
		s.sketch = NewTDigest(100)
	}
	s.sketch.Add(p.Value, 1)
}

func (s *telemetryStats) merge(o telemetryStats) error {
	if o.Attempts == 0 {
		return nil
	}
	if s.Attempts == 0 {
		*s = o
		// Never alias a mutable sketch across the finest and coarser deltas.
		if o.sketch != nil {
			s.Digest = o.sketch.encodeRaw()
			s.sketch = nil
		}
		return nil
	}
	if o.FirstNano < s.FirstNano || (o.FirstNano == s.FirstNano && (o.FirstIngest < s.FirstIngest || (o.FirstIngest == s.FirstIngest && o.FirstOrdinal < s.FirstOrdinal))) {
		s.First = o.First
		s.FirstNano = o.FirstNano
		s.FirstIngest = o.FirstIngest
		s.FirstOrdinal = o.FirstOrdinal
	}
	if o.LastNano > s.LastNano || (o.LastNano == s.LastNano && (o.LastIngest > s.LastIngest || (o.LastIngest == s.LastIngest && o.LastOrdinal > s.LastOrdinal))) {
		s.Last = o.Last
		s.LastNano = o.LastNano
		s.LastIngest = o.LastIngest
		s.LastOrdinal = o.LastOrdinal
	}
	s.Attempts += o.Attempts
	s.Loss += o.Loss
	if o.N == 0 {
		return nil
	}
	if s.N == 0 {
		s.Min = o.Min
		s.Max = o.Max
		s.Mean = o.Mean
		s.M2 = o.M2
	} else {
		d := o.Mean - s.Mean
		n := s.N + o.N
		s.M2 += o.M2 + d*d*(float64(s.N)*float64(o.N)/float64(n))
		s.Mean += d * float64(o.N) / float64(n)
		s.Min = min(s.Min, o.Min)
		s.Max = max(s.Max, o.Max)
	}
	s.N += o.N
	s.Sum += o.Sum
	if s.sketch == nil {
		var err error
		s.sketch, err = DecodeTDigest(s.Digest)
		if err != nil && len(s.Digest) != 0 {
			return err
		}
		if s.sketch == nil {
			s.sketch = NewTDigest(100)
		}
	}
	od := o.sketch
	if od == nil && len(o.Digest) > 0 {
		var err error
		od, err = DecodeTDigest(o.Digest)
		if err != nil {
			return err
		}
	}
	if od != nil {
		s.sketch.Merge(od)
	}
	return nil
}

func (s telemetryStats) encode() (string, error) {
	if s.sketch != nil {
		s.Digest = s.sketch.encodeRaw()
	}
	b, e := json.Marshal(s)
	return string(b), e
}

type telemetrySummaryKey struct {
	segment, series    string
	resolution, bucket int64
}

// SummarizeNext consumes exactly one bounded ingress batch. Every output tier
// is merged from that batch's finest-bucket delta in the same transaction as
// the cursor. Segment-qualified cells are immutable once their input is sealed;
// late arrivals create supplementary cells, never rewrite historical archives.
func (t *Telemetry) SummarizeNext(ctx context.Context) (int, error) {
	release, err := t.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	tx, err := t.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	before, err := t.progress(ctx, tx, "summary")
	if err != nil {
		return 0, err
	}
	var next int64
	var expected int
	err = tx.QueryRowContext(ctx, t.bind("SELECT ingest_id,points FROM "+t.table("receipts")+" WHERE ingest_id>? ORDER BY ingest_id LIMIT 1"), before).Scan(&next, &expected)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, t.bind("SELECT h.ordinal,h.series_id,h.observed,h.value,h.segment_id,s.metric,COALESCE(e.through_id,0) FROM "+t.table("hot")+" h JOIN "+t.table("series")+" s ON s.id=h.series_id LEFT JOIN "+t.table("erasures")+" e ON e.series_id=s.id WHERE h.ingest_id=? ORDER BY h.ordinal"), next)
	if err != nil {
		return 0, err
	}
	base := make(map[telemetrySummaryKey]*telemetryStats)
	n := 0
	for rows.Next() {
		var ordinal int
		var sid, seg, name string
		var ns int64
		var value float64
		var erasedThrough int64
		if err = rows.Scan(&ordinal, &sid, &ns, &value, &seg, &name, &erasedThrough); err != nil {
			break
		}
		n++
		if next <= erasedThrough {
			continue
		}
		k := telemetrySummaryKey{seg, sid, int64(10 * time.Second), time.Unix(0, ns).UTC().Truncate(10 * time.Second).UnixNano()}
		if base[k] == nil {
			base[k] = &telemetryStats{}
		}
		base[k].add(Point{MetricName: name, Timestamp: time.Unix(0, ns), Value: value}, next, ordinal)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if readErr != nil {
		return 0, readErr
	}
	if n != expected {
		return 0, errors.New("telemetry summary source disappeared before coverage")
	}
	all := make(map[telemetrySummaryKey]*telemetryStats, len(base)*4)
	for k, v := range base {
		all[k] = v
		for _, r := range []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour} {
			ck := k
			ck.resolution = int64(r)
			ck.bucket = time.Unix(0, k.bucket).UTC().Truncate(r).UnixNano()
			if all[ck] == nil {
				all[ck] = &telemetryStats{}
			}
			if err = all[ck].merge(*v); err != nil {
				return 0, err
			}
		}
	}
	for k, v := range all {
		var raw string
		err = tx.QueryRowContext(ctx, t.bind("SELECT stats FROM "+t.table("summary")+" WHERE segment_id=? AND series_id=? AND resolution=? AND bucket=?"), k.segment, k.series, k.resolution, k.bucket).Scan(&raw)
		if err == nil {
			var old telemetryStats
			if err = json.Unmarshal([]byte(raw), &old); err != nil {
				return 0, err
			}
			if err = old.merge(*v); err != nil {
				return 0, err
			}
			v = &old
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		encoded, e := v.encode()
		if e != nil {
			return 0, e
		}
		q := "INSERT INTO " + t.table("summary") + " (segment_id,series_id,resolution,bucket,stats) VALUES (?,?,?,?,?)"
		if t.s.cfg.Driver == DriverMySQL {
			q += " ON DUPLICATE KEY UPDATE stats=VALUES(stats)"
		} else {
			q += " ON CONFLICT(segment_id,series_id,resolution,bucket) DO UPDATE SET stats=excluded.stats"
		}
		if _, err = tx.ExecContext(ctx, t.bind(q), k.segment, k.series, k.resolution, k.bucket, encoded); err != nil {
			return 0, err
		}
	}
	if err = t.advance(ctx, tx, "summary", before, next); err != nil {
		return 0, err
	}
	if err = t.fault("summary-before-commit"); err != nil {
		return 0, err
	}
	// A traffic-only batch still advances the cursor and counts as work.
	return max(n, 1), tx.Commit()
}

func telemetryResolution(age time.Duration) time.Duration {
	switch {
	case age <= 2*time.Hour:
		return 10 * time.Second
	case age <= 24*time.Hour:
		return 5 * time.Minute
	case age <= 7*24*time.Hour:
		return 15 * time.Minute
	default:
		return time.Hour
	}
}

type TelemetryQueryResult struct {
	stats            map[aggregateGroupKey]*telemetryStats
	Points           []AggregatePoint
	Resolution       time.Duration
	AccountedThrough int64
	AcceptedThrough  int64
	CoverageStart    time.Time
	CoverageEnd      time.Time
	BoundaryAdjusted bool
}

// Aggregate reads summary SQL only. It never opens an archive. Partial bucket
// boundaries and intervals finer than retained precision fail explicitly.
func (t *Telemetry) Aggregate(ctx context.Context, q AggregateQuery, now time.Time) (TelemetryQueryResult, error) {
	return t.aggregate(ctx, q, now, false)
}

// Chart includes a bounded unsummarized hot tail in the same SQL snapshot.
// Recent partial boundaries use exact hot points. Older partial boundaries
// return the retained bucket intersection and disclose adjusted coverage.
// No archive is opened by this path, even when summary processing is behind.
func (t *Telemetry) Chart(ctx context.Context, q AggregateQuery, now time.Time) (TelemetryQueryResult, error) {
	return t.aggregate(ctx, q, now, true)
}

func (t *Telemetry) aggregate(ctx context.Context, q AggregateQuery, now time.Time, chart bool) (TelemetryQueryResult, error) {
	var out TelemetryQueryResult
	if err := q.Validate(); err != nil {
		return out, err
	}
	originalMetric := q.MetricName
	if q.MetricName == sqliteVirtualPingLossMetric {
		q.MetricName = sqliteMergedPingLatencyMetric
		q.Aggregation = pingLossPhysicalAggregation(q.Aggregation)
	}
	resolution := telemetryResolution(now.Sub(q.Start))
	if t.legacyUntil > 0 && q.Start.UnixNano() <= t.legacyUntil && q.Interval < t.legacyResolution {
		return out, ErrTelemetryCoverage
	}
	out.Resolution = resolution
	if q.Interval < resolution || q.Interval%resolution != 0 || (!chart && (q.Start.UnixNano()%int64(resolution) != 0 || (q.End.UnixNano()+1)%int64(resolution) != 0)) {
		return out, ErrTelemetryCoverage
	}
	if q.Aggregation == AggRate || q.Aggregation == aggPingLossRate {
		return out, ErrTelemetryCoverage
	}
	// Hold a read transaction so the coverage cursor and cells are one snapshot.
	tx, shared := ctx.Value(telemetrySnapshotKey{}).(*sql.Tx)
	if !shared {
		var err error
		tx, err = t.s.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
		if err != nil {
			return out, err
		}
		defer tx.Rollback()
	}
	var err error
	out.AccountedThrough, err = t.progress(ctx, tx, "summary")
	if err != nil {
		return out, err
	}
	out.AcceptedThrough, err = t.progress(ctx, tx, "ingest")
	if err != nil {
		return out, err
	}
	boundaryResolution := resolution
	if t.legacyUntil > 0 && q.Start.UnixNano() <= t.legacyUntil {
		boundaryResolution = telemetryLCM(resolution, t.legacyResolution)
		out.Resolution = boundaryResolution
	}
	first := q.Start.Truncate(boundaryResolution)
	if first.Before(q.Start) {
		first = first.Add(boundaryResolution)
	}
	lastEnd := q.End.Add(time.Nanosecond).Truncate(boundaryResolution).Add(-time.Nanosecond)
	out.CoverageStart, out.CoverageEnd = q.Start, q.End
	cutoff := now.Add(-t.cfg.RawWindow)
	if chart && q.Start.Before(cutoff) && first.After(q.Start) {
		out.CoverageStart, out.BoundaryAdjusted = first, true
	}
	if chart && q.End.Before(cutoff) && lastEnd.Before(q.End) {
		out.CoverageEnd, out.BoundaryAdjusted = lastEnd, true
	}
	sqlText := "SELECT s.metric,s.entity,s.tags,b.bucket,b.stats FROM " + t.table("summary") + " b JOIN " + t.table("series") + " s ON s.id=b.series_id JOIN " + t.table("segments") + " a ON a.id=b.segment_id LEFT JOIN " + t.table("erasures") + " e ON e.series_id=s.id WHERE s.metric=? AND ((a.state<>'legacy' AND b.resolution=?) OR (a.state='legacy' AND b.resolution<=?)) AND b.bucket>=? AND b.bucket+b.resolution<=? AND (e.through_id IS NULL OR a.first_id>e.through_id)"
	args := []any{q.MetricName, int64(resolution), int64(q.Interval), first.UnixNano(), lastEnd.Add(time.Nanosecond).UnixNano()}
	filter, filterArgs, err := t.seriesFilter(ctx, q.Query, "s")
	if err != nil {
		return out, err
	}
	sqlText += filter
	args = append(args, filterArgs...)
	sqlText += " ORDER BY b.bucket,s.id,b.segment_id LIMIT 100001"
	rows, err := tx.QueryContext(ctx, t.bind(sqlText), args...)
	if err != nil {
		return out, err
	}
	groups := make(map[aggregateGroupKey]*telemetryStats)
	tagsByGroup := make(map[aggregateGroupKey]map[string]string)
	n := 0
	decodedBytes := 0
	for rows.Next() {
		n++
		if n > 100000 {
			rows.Close()
			return out, ErrTelemetryBackpressure
		}
		var name, entity, tagsJSON, raw string
		var ns int64
		if err = rows.Scan(&name, &entity, &tagsJSON, &ns, &raw); err != nil {
			break
		}
		decodedBytes += len(raw) + len(tagsJSON) + 256
		if decodedBytes > 32<<20 {
			rows.Close()
			return out, ErrTelemetryBackpressure
		}
		var tags map[string]string
		if err = json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
			break
		}
		matches := true
		for k, v := range q.Tags {
			if tags[k] != v {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		var stats telemetryStats
		if err = json.Unmarshal([]byte(raw), &stats); err != nil {
			break
		}
		key := aggregateGroupKey{bucket: time.Unix(0, ns).UTC().Truncate(q.Interval).UnixNano()}
		if q.PreserveSeries {
			h, _, e := tagsFingerprint(tags)
			if e != nil {
				err = e
				break
			}
			key.aggregateSeriesKey = aggregateSeriesKey{name, entity, h}
		}
		if groups[key] == nil {
			groups[key] = &telemetryStats{}
			tagsByGroup[key] = tags
		}
		if err = groups[key].merge(stats); err != nil {
			break
		}
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if readErr != nil {
		return out, readErr
	}
	if chart {
		// Interior summarized cells and hot points are disjoint by durable cursor.
		// Exact boundary points are read irrespective of that cursor.
		hotSQL := "SELECT s.metric,s.entity,s.tags,h.observed,h.value,h.ingest_id,h.ordinal FROM " + t.table("hot") + " h JOIN " + t.table("series") + " s ON s.id=h.series_id LEFT JOIN " + t.table("erasures") + " e ON e.series_id=s.id WHERE s.metric=? AND h.observed>=? AND h.observed<=? AND (e.through_id IS NULL OR h.ingest_id>e.through_id) AND (h.ingest_id>? OR h.observed<? OR h.observed>?)" + filter + " ORDER BY h.ingest_id,h.ordinal LIMIT 10001"
		hotArgs := []any{q.MetricName, q.Start.UnixNano(), q.End.UnixNano(), out.AccountedThrough, first.UnixNano(), lastEnd.UnixNano()}
		hotArgs = append(hotArgs, filterArgs...)
		hr, e := tx.QueryContext(ctx, t.bind(hotSQL), hotArgs...)
		if e != nil {
			return out, e
		}
		count := 0
		for hr.Next() {
			count++
			if count > 10000 {
				hr.Close()
				return out, ErrTelemetryBackpressure
			}
			var p Point
			var ns, ingest int64
			var ordinal int
			var rawTags string
			if e = hr.Scan(&p.MetricName, &p.EntityID, &rawTags, &ns, &p.Value, &ingest, &ordinal); e != nil {
				break
			}
			p.Timestamp = time.Unix(0, ns).UTC()
			if p.Timestamp.Before(cutoff) {
				// Pending old input must not silently disappear, nor turn an
				// online query into an unbounded raw-history fallback.
				if ingest > out.AccountedThrough {
					hr.Close()
					return out, ErrTelemetryCoverage
				}
				continue
			}
			if e = json.Unmarshal([]byte(rawTags), &p.Tags); e != nil {
				break
			}
			key := aggregateGroupKey{bucket: p.Timestamp.Truncate(q.Interval).UnixNano()}
			if q.PreserveSeries {
				h, _, fingerprintErr := tagsFingerprint(p.Tags)
				if fingerprintErr != nil {
					e = fingerprintErr
					break
				}
				key.aggregateSeriesKey = aggregateSeriesKey{p.MetricName, p.EntityID, h}
			}
			if groups[key] == nil {
				groups[key] = &telemetryStats{}
				tagsByGroup[key] = p.Tags
			}
			groups[key].add(p, ingest, ordinal)
		}
		re := hr.Err()
		hr.Close()
		if e != nil {
			return out, e
		}
		if re != nil {
			return out, re
		}
	}
	keys := make([]aggregateGroupKey, 0, len(groups))
	out.stats = groups
	for k := range groups {
		keys = append(keys, k)
	}
	sortAggregateGroupKeys(keys)
	for _, k := range keys {
		st := groups[k]
		v, e := st.value(q.Aggregation)
		if e != nil {
			return out, e
		}
		entity := q.EntityID
		if q.PreserveSeries {
			entity = k.entityID
		}
		out.Points = append(out.Points, AggregatePoint{MetricName: originalMetric, EntityID: entity, Bucket: time.Unix(0, k.bucket).UTC(), Value: v, Count: int(st.Attempts), Tags: tagsByGroup[k]})
	}
	if q.Order == OrderDesc {
		sort.SliceStable(out.Points, func(i, j int) bool { return out.Points[i].Bucket.After(out.Points[j].Bucket) })
	}
	out.Points = pageBuckets(out.Points, q.BucketLimit, q.BucketOffset)
	if shared {
		return out, nil
	}
	return out, tx.Commit()
}

func (s telemetryStats) value(a Aggregation) (float64, error) {
	switch a {
	case AggCount:
		return float64(s.Attempts), nil
	case AggFirst:
		return s.First, nil
	case AggLast:
		return s.Last, nil
	case AggSum:
		return s.Sum, nil
	case AggMin:
		if s.N == 0 {
			return 0, nil
		}
		return s.Min, nil
	case AggMax:
		if s.N == 0 {
			return 0, nil
		}
		return s.Max, nil
	case AggAvg:
		if s.N == 0 {
			return 0, nil
		}
		return s.Sum / float64(s.N), nil
	case AggStdDev:
		if s.N == 0 {
			return 0, nil
		}
		return math.Sqrt(max(0, s.M2) / float64(s.N)), nil
	case aggPingLossAvg:
		if s.Attempts == 0 {
			return 0, nil
		}
		return float64(s.Loss) / float64(s.Attempts), nil
	case aggPingLossSum:
		return float64(s.Loss), nil
	case aggPingLossMin:
		if s.Attempts > 0 && s.Loss == s.Attempts {
			return 1, nil
		}
		return 0, nil
	case aggPingLossMax:
		if s.Loss > 0 {
			return 1, nil
		}
		return 0, nil
	case aggPingLossFirst:
		if s.First < 0 {
			return 1, nil
		}
		return 0, nil
	case aggPingLossLast:
		if s.Last < 0 {
			return 1, nil
		}
		return 0, nil
	case aggPingLossStdDev:
		if s.Attempts == 0 {
			return 0, nil
		}
		p := float64(s.Loss) / float64(s.Attempts)
		return math.Sqrt(p * (1 - p)), nil
	}
	if f, ok := parsePingLossPercentile(a); ok {
		return pingLossPercentile(s.Attempts, s.Loss, f), nil
	}
	if f, ok := parsePercentile(a); ok {
		if s.N == 0 {
			return 0, nil
		}
		td := s.sketch
		if td == nil {
			var err error
			td, err = DecodeTDigest(s.Digest)
			if err != nil {
				return 0, err
			}
		}
		return td.Quantile(f), nil
	}
	return 0, fmt.Errorf("%w: telemetry aggregation %s", ErrTelemetryCoverage, a)
}

package metric

import (
	"context"
	"database/sql"
	"time"
)

type telemetrySnapshotKey struct{}

func telemetryInterval(start, now time.Time, interval time.Duration) time.Duration {
	r := telemetryResolution(now.Sub(start))
	interval = max(interval, r)
	if remainder := interval % r; remainder != 0 {
		interval += r - remainder
	}
	return interval
}

func (t *Telemetry) compatibleInterval(start, now time.Time, interval time.Duration) time.Duration {
	interval = telemetryInterval(start, now, interval)
	if t.legacyUntil > 0 && start.UnixNano() <= t.legacyUntil && t.legacyResolution > 0 {
		interval = telemetryLCM(interval, t.legacyResolution)
	}
	return interval
}

func (t *Telemetry) SeriesBatch(ctx context.Context, queries []AggregateQuery, now time.Time) ([][]AggregatePoint, error) {
	if len(queries) > 64 {
		return nil, ErrTelemetryBackpressure
	}
	tx, err := t.s.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ctx = context.WithValue(ctx, telemetrySnapshotKey{}, tx)
	out := make([][]AggregatePoint, len(queries))
	for i, q := range queries {
		r, e := t.Chart(ctx, q, now)
		if e != nil {
			return nil, e
		}
		out[i] = r.Points
	}
	return out, tx.Commit()
}

// PingSummary merges all retained statistical cells for each task before
// evaluating percentiles and variance. Averaging per-bucket percentiles or
// weighting latency averages by failed attempts would change their meaning.
func (t *Telemetry) PingSummary(ctx context.Context, q AggregateQuery, now time.Time) (SeriesSummary, error) {
	q.Aggregation = AggCount
	q.MetricName = sqliteMergedPingLatencyMetric
	q.PreserveSeries = true
	q.Interval = t.compatibleInterval(q.Start, now, max(q.Interval, 10*time.Second))
	r, err := t.Chart(ctx, q, now)
	if err != nil {
		return SeriesSummary{}, err
	}
	stats := map[aggregateSeriesKey]*telemetryStats{}
	tags := map[aggregateSeriesKey]map[string]string{}
	for key, value := range r.stats {
		if stats[key.aggregateSeriesKey] == nil {
			stats[key.aggregateSeriesKey] = &telemetryStats{}
		}
		if err = stats[key.aggregateSeriesKey].merge(*value); err != nil {
			return SeriesSummary{}, err
		}
	}
	for _, p := range r.Points {
		h, _, e := tagsFingerprint(p.Tags)
		if e != nil {
			return SeriesSummary{}, e
		}
		tags[aggregateSeriesKey{p.MetricName, p.EntityID, h}] = p.Tags
	}
	var out SeriesSummary
	for key, s := range stats {
		for _, view := range []struct {
			a    Aggregation
			dest *[]AggregatePoint
			name string
		}{{AggAvg, &out.Avg, sqliteMergedPingLatencyMetric}, {AggMin, &out.Min, sqliteMergedPingLatencyMetric}, {AggMax, &out.Max, sqliteMergedPingLatencyMetric}, {AggLast, &out.Last, sqliteMergedPingLatencyMetric}, {AggP50, &out.P50, sqliteMergedPingLatencyMetric}, {AggP99, &out.P99, sqliteMergedPingLatencyMetric}, {AggStdDev, &out.StdDev, sqliteMergedPingLatencyMetric}, {aggPingLossAvg, &out.Loss, sqliteVirtualPingLossMetric}} {
			value, e := s.value(view.a)
			if e != nil {
				return out, e
			}
			*view.dest = append(*view.dest, AggregatePoint{MetricName: view.name, EntityID: key.entityID, Bucket: time.Unix(0, s.LastNano).UTC(), Value: value, Count: int(s.Attempts), Tags: tags[key]})
		}
	}
	return out, nil
}

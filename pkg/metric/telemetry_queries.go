package metric

import (
	"context"
	"database/sql"
	"time"
)

type telemetrySnapshotKey struct{}

type telemetryChartBudgetKey struct{}
type telemetryChartBudget struct{ points, bytes int }

// ChartBatchBounded shares one allocation budget across every query. Export
// manifests must reject excessive cardinality before materializing the result,
// not after the worker has already exhausted its memory cgroup.
func (t *Telemetry) ChartBatchBounded(ctx context.Context, queries []AggregateQuery, now time.Time, points, bytes int) ([]TelemetryQueryResult, error) {
	if points < 1 || bytes < 1 {
		return nil, ErrInvalidArgument
	}
	return t.ChartBatch(context.WithValue(ctx, telemetryChartBudgetKey{}, &telemetryChartBudget{points, bytes}), queries, now)
}

func reserveChartMemory(ctx context.Context, points, bytes int) error {
	if b, ok := ctx.Value(telemetryChartBudgetKey{}).(*telemetryChartBudget); ok {
		if points > b.points || bytes > b.bytes {
			return ErrTelemetryBackpressure
		}
		b.points -= points
		b.bytes -= bytes
	}
	return nil
}

func telemetryInterval(start, now time.Time, interval time.Duration) time.Duration {
	// Output-grid compatibility is independent of unrelated retained history.
	// The query planner resolves actual source precision for each ownership span.
	r := 10 * time.Second
	interval = max(interval, r)
	if remainder := interval % r; remainder != 0 {
		interval += r - remainder
	}
	return interval
}

func (t *Telemetry) compatibleInterval(start, now time.Time, interval time.Duration) time.Duration {
	return telemetryInterval(start, now, interval)
}

// PingWindow returns the interval which can be answered from retained cells.
// Historical edges move inward to the output grid. Recent hot edges remain
// exact. Callers return these actual bounds with the statistics so the probe
// denominator always describes the advertised window.
func (t *Telemetry) PingWindow(start, end time.Time, interval time.Duration, now time.Time) (time.Time, time.Time, error) {
	if end.Before(start) || interval <= 0 {
		return start, end, ErrInvalidArgument
	}
	interval = t.compatibleInterval(start, now, interval)
	cutoff := now.Add(-t.cfg.RawWindow)
	if start.Before(cutoff) {
		aligned := alignTime(start.UTC(), interval)
		if aligned.Before(start) {
			aligned = aligned.Add(interval)
		}
		start = aligned
	}
	if end.Before(cutoff) {
		end = alignTime(end.UTC().Add(time.Nanosecond), interval).Add(-time.Nanosecond)
	}
	if end.Before(start) {
		return start, end, ErrTelemetryCoverage
	}
	return start, end, nil
}

func (t *Telemetry) SeriesBatch(ctx context.Context, queries []AggregateQuery, now time.Time) ([][]AggregatePoint, error) {
	results, err := t.ChartBatch(ctx, queries, now)
	if err != nil {
		return nil, err
	}
	out := make([][]AggregatePoint, len(results))
	for i, r := range results {
		out[i] = r.Points
	}
	return out, nil
}

// ChartBatch preserves precision/coverage and points in the same read snapshot.
func (t *Telemetry) ChartBatch(ctx context.Context, queries []AggregateQuery, now time.Time) ([]TelemetryQueryResult, error) {
	if len(queries) > 64 {
		return nil, ErrTelemetryBackpressure
	}
	tx, err := t.s.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ctx = context.WithValue(ctx, telemetrySnapshotKey{}, tx)
	out := make([]TelemetryQueryResult, len(queries))
	for i, q := range queries {
		r, e := t.Chart(ctx, q, now)
		if e != nil {
			return nil, e
		}
		out[i] = r
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
	// A statistics card has no per-bucket gap representation. Returning only
	// the retained subset would silently change its probe denominator and
	// percentiles. The caller must disclose unavailable coverage instead.
	if len(r.Gaps) > 0 {
		return SeriesSummary{}, ErrTelemetryCoverage
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

package metric

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// TelemetrySourceSpan describes retained precision, independently of the
// requested output grid. Missing spans are unknown observations, never zeros.
type TelemetrySourceSpan struct {
	MetricName string
	EntityID   string
	Tags       map[string]string
	Start, End time.Time // half-open
	Resolution time.Duration
	Reason     string
}

type telemetryPlanCell struct {
	segment, series, state, name, entity string
	tags                                 map[string]string
	bucket, resolution                   int64
	through                              int64
	stats                                telemetryStats
}

type telemetryPlanOwner struct{ segment, series string }
type telemetryPlanRange struct{ start, end, through int64 }
type telemetryChartPlan struct {
	selected map[telemetryPlanOwner][]telemetryPlanRange
	legacy   map[string][]telemetryPlanRange
	missing  map[aggregateGroupKey]bool
}

func coalesceTelemetrySpans(spans []TelemetrySourceSpan) []TelemetrySourceSpan {
	type item struct {
		span TelemetrySourceSpan
		key  string
	}
	items := make([]item, 0, len(spans))
	for _, s := range spans {
		h, _, _ := tagsFingerprint(s.Tags)
		items = append(items, item{s, fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s", s.MetricName, s.EntityID, h, s.Resolution, s.Reason)})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].key == items[j].key {
			return items[i].span.Start.Before(items[j].span.Start)
		}
		return items[i].key < items[j].key
	})
	out := make([]TelemetrySourceSpan, 0, len(items))
	prior := ""
	for _, v := range items {
		if len(out) > 0 && prior == v.key && !v.span.Start.After(out[len(out)-1].End) {
			if v.span.End.After(out[len(out)-1].End) {
				out[len(out)-1].End = v.span.End
			}
		} else {
			out = append(out, v.span)
		}
		prior = v.key
	}
	return out
}

func telemetryPlanGroup(q AggregateQuery, name, entity string, tags map[string]string, at int64) (aggregateGroupKey, error) {
	k := aggregateGroupKey{bucket: time.Unix(0, at).UTC().Truncate(q.Interval).UnixNano()}
	if q.PreserveSeries {
		h, _, err := tagsFingerprint(tags)
		if err != nil {
			return k, err
		}
		k.aggregateSeriesKey = aggregateSeriesKey{name, entity, h}
	}
	return k, nil
}

func (p *telemetryChartPlan) contains(owner telemetryPlanOwner, at int64) bool {
	rows := p.selected[owner]
	i := sort.Search(len(rows), func(i int) bool { return rows[i].end > at })
	return i < len(rows) && rows[i].start <= at
}

func (p *telemetryChartPlan) coversRaw(owner telemetryPlanOwner, at, ingest int64) bool {
	if p.contains(owner, at) {
		return true
	}
	for _, r := range p.legacy[owner.series] {
		if ingest <= r.through && at >= r.start && at < r.end {
			return true
		}
	}
	return false
}

// The online producer commits every tier with exactly the same input cursor.
// Those tiers are alternate representations of one segment/series owner.
// Legacy cells instead own disjoint input: an in-progress promotion may have
// an additive coarse cell overlapping remaining fine cells in wall-clock time.
// Never apply online tier deduplication to that canonical legacy ownership.
func (t *Telemetry) planChart(ctx context.Context, tx *sql.Tx, q AggregateQuery, now time.Time, chart bool, out *TelemetryQueryResult, groups map[aggregateGroupKey]*telemetryStats, tagsByGroup map[aggregateGroupKey]map[string]string) (*telemetryChartPlan, error) {
	p := &telemetryChartPlan{selected: map[telemetryPlanOwner][]telemetryPlanRange{}, legacy: map[string][]telemetryPlanRange{}, missing: map[aggregateGroupKey]bool{}}
	filter, args, err := t.seriesFilter(ctx, q.Query, "s")
	if err != nil {
		return nil, err
	}
	query := "SELECT b.segment_id,b.series_id,a.state,a.last_id,s.metric,s.entity,s.tags,b.bucket,b.resolution,b.stats FROM " + t.table("summary") + " b JOIN " + t.table("series") + " s ON s.id=b.series_id JOIN " + t.table("segments") + " a ON a.id=b.segment_id LEFT JOIN " + t.table("erasures") + " e ON e.series_id=s.id WHERE s.metric=? AND b.bucket<=? AND b.bucket+b.resolution>? AND (e.through_id IS NULL OR a.first_id>e.through_id)" + filter + " ORDER BY b.segment_id,b.series_id,b.resolution DESC,b.bucket LIMIT 100001"
	args = append([]any{q.MetricName, q.End.UnixNano(), q.Start.UnixNano()}, args...)
	rows, err := tx.QueryContext(ctx, t.bind(query), args...)
	if err != nil {
		return nil, err
	}
	owners := map[telemetryPlanOwner][]telemetryPlanCell{}
	count, bytes := 0, 0
	for rows.Next() {
		var c telemetryPlanCell
		var tags, raw string
		if err = rows.Scan(&c.segment, &c.series, &c.state, &c.through, &c.name, &c.entity, &tags, &c.bucket, &c.resolution, &raw); err != nil {
			break
		}
		count++
		bytes += len(raw) + len(tags) + 256
		if count > 100000 || bytes > 32<<20 {
			err = ErrTelemetryBackpressure
			break
		}
		if c.resolution <= 0 || c.bucket > q.End.UnixNano() || c.bucket+c.resolution <= c.bucket {
			err = ErrTelemetryCoverage
			break
		}
		if err = json.Unmarshal([]byte(tags), &c.tags); err != nil {
			break
		}
		matches := true
		for k, v := range q.Tags {
			if c.tags[k] != v {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		if err = json.Unmarshal([]byte(raw), &c.stats); err != nil {
			break
		}
		owner := telemetryPlanOwner{c.segment, c.series}
		owners[owner] = append(owners[owner], c)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	cutoff := now.Add(-t.cfg.RawWindow).UnixNano()
	for owner, cells := range owners {
		var chosen []telemetryPlanCell
		for _, c := range cells {
			if c.stats.Attempts == 0 || c.stats.LastNano < q.Start.UnixNano() || c.stats.FirstNano > q.End.UnixNano() {
				continue
			}
			if time.Duration(c.resolution) > q.Interval {
				continue
			}
			first, last := c.stats.FirstNano, c.stats.LastNano
			if first < q.Start.UnixNano() || last > q.End.UnixNano() {
				continue
			}
			// The complete observed contents must fit a single output bucket.
			// Coarse statistics are never interpolated or divided into fake points.
			if !time.Unix(0, first).UTC().Truncate(q.Interval).Equal(time.Unix(0, last).UTC().Truncate(q.Interval)) {
				continue
			}
			if !chart && (c.bucket < q.Start.UnixNano() || c.bucket+c.resolution > q.End.UnixNano()+1 || time.Duration(c.resolution) > q.Interval) {
				continue
			}
			if c.state != "legacy" {
				covered := false
				for _, prior := range chosen {
					if c.bucket >= prior.bucket && c.bucket+c.resolution <= prior.bucket+prior.resolution {
						covered = true
						break
					}
					if c.bucket < prior.bucket+prior.resolution && c.bucket+c.resolution > prior.bucket {
						return nil, fmt.Errorf("%w: non-nested source representations", ErrTelemetryCoverage)
					}
				}
				if covered {
					continue
				}
			}
			chosen = append(chosen, c)
			k, err := telemetryPlanGroup(q, c.name, c.entity, c.tags, first)
			if err != nil {
				return nil, err
			}
			if groups[k] == nil {
				groups[k] = &telemetryStats{}
				tagsByGroup[k] = c.tags
			}
			if err = groups[k].merge(c.stats); err != nil {
				return nil, err
			}
			p.selected[owner] = append(p.selected[owner], telemetryPlanRange{c.bucket, c.bucket + c.resolution, c.through})
			if c.state == "legacy" {
				p.legacy[c.series] = append(p.legacy[c.series], telemetryPlanRange{c.bucket, c.bucket + c.resolution, c.through})
			}
			out.Resolution = max(out.Resolution, time.Duration(c.resolution))
			out.Sources = append(out.Sources, TelemetrySourceSpan{MetricName: c.name, EntityID: c.entity, Tags: c.tags, Start: time.Unix(0, max(c.bucket, q.Start.UnixNano())).UTC(), End: time.Unix(0, min(c.bucket+c.resolution, q.End.UnixNano()+1)).UTC(), Resolution: time.Duration(c.resolution)})
		}
		for _, c := range cells {
			if c.stats.Attempts == 0 || c.stats.LastNano < q.Start.UnixNano() || c.stats.FirstNano > q.End.UnixNano() {
				continue
			}
			covered := false
			var represented int64
			for _, selected := range chosen {
				if c.state == "legacy" {
					if selected.bucket == c.bucket && selected.resolution == c.resolution {
						covered = true
						break
					}
				} else {
					if selected.bucket <= c.bucket && selected.bucket+selected.resolution >= c.bucket+c.resolution {
						covered = true
						break
					}
					if selected.bucket >= c.bucket && selected.bucket+selected.resolution <= c.bucket+c.resolution {
						represented += selected.stats.Attempts
					}
				}
			}
			// For alternate online tiers, equal represented counts prove that no
			// source observation remains, including legitimate empty fine buckets.
			if covered || c.state != "legacy" && represented == c.stats.Attempts {
				continue
			}
			from, through := max(c.stats.FirstNano, q.Start.UnixNano()), min(c.stats.LastNano, q.End.UnixNano())
			if chart && from >= cutoff {
				continue
			} // bounded exact hot boundary below
			if !chart {
				return nil, ErrTelemetryCoverage
			}
			through = min(through, cutoff-1)
			if through < from {
				continue
			}
			start := time.Unix(0, from).UTC().Truncate(q.Interval)
			for at := start; at.UnixNano() <= through; at = at.Add(q.Interval) {
				k, err := telemetryPlanGroup(q, c.name, c.entity, c.tags, at.UnixNano())
				if err != nil {
					return nil, err
				}
				p.missing[k] = true
				if len(p.missing) > 100000 {
					return nil, ErrTelemetryBackpressure
				}
			}
			out.Gaps = append(out.Gaps, TelemetrySourceSpan{MetricName: c.name, EntityID: c.entity, Tags: c.tags, Start: time.Unix(0, from).UTC(), End: time.Unix(0, through+1).UTC(), Resolution: time.Duration(c.resolution), Reason: "retained_precision_or_boundary"})
		}
		// Legacy promotion can leave overlapping, additive source cells. Raw
		// exclusion uses their union, whose ends must be monotonic for Search.
		ranges := p.selected[owner]
		sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
		union := ranges[:0]
		for _, r := range ranges {
			if len(union) > 0 && r.start <= union[len(union)-1].end {
				union[len(union)-1].end = max(union[len(union)-1].end, r.end)
			} else {
				union = append(union, r)
			}
		}
		p.selected[owner] = union
	}
	return p, nil
}

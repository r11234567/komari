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

func telemetryPlanGroup(ctx context.Context, q AggregateQuery, name, entity string, tags map[string]string, at int64) (aggregateGroupKey, error) {
	k := aggregateGroupKey{bucket: alignTime(time.Unix(0, at).UTC(), q.Interval).UnixNano()}
	if card, _ := ctx.Value(telemetryStatisticsCardKey{}).(bool); card {
		// Selection still uses the caller's exact window and output precision.
		// A statistics card needs one distribution per task, not every chart
		// bucket as an intermediate result. It never exposes this internal key.
		k.bucket = 0
	}
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
	// Online tiers are nested within UTC hours. Include the complete boundary
	// days so children outside the request can prove that a parent has no
	// unrepresented observations inside it. Legacy cells own additive input and
	// must retain their original window selection.
	boundaryStart := alignTime(q.Start.UTC(), 24*time.Hour).UnixNano()
	boundaryEnd := alignTime(q.End.UTC(), 24*time.Hour).Add(24 * time.Hour).UnixNano()
	// Select owners by their observed-time membership before reading cells.
	// The summary series index starts with resolution, so filtering only on
	// bucket+b.resolution otherwise walks every retained cell of the series.
	// Membership is updated atomically with ingestion/migration and survives
	// raw retirement; creation time cannot substitute for late observations.
	cutoff := now.Add(-t.cfg.RawWindow).UnixNano()
	card, _ := ctx.Value(telemetryStatisticsCardKey{}).(bool)
	processOwner := func(owner telemetryPlanOwner, cells []telemetryPlanCell) error {
		var sources, gaps []TelemetrySourceSpan
		proofBytes := 0
		defer func() { releaseChartMemory(ctx, proofBytes) }()
		var ownerStats telemetryStats
		var ownerKey aggregateGroupKey
		var ownerTags map[string]string
		ownerAllocated := false
		defer func() {
			if ownerAllocated {
				releaseChartMemory(ctx, 512+chartStatsBytes(&ownerStats))
			}
		}()
		var chosen []telemetryPlanCell
		var outside []telemetryPlanCell
		for _, c := range cells {
			if c.stats.Attempts == 0 {
				continue
			}
			if c.stats.LastNano < q.Start.UnixNano() || c.stats.FirstNano > q.End.UnixNano() {
				if c.state != "legacy" {
					contained := false
					for _, prior := range outside {
						if c.bucket >= prior.bucket && c.bucket+c.resolution <= prior.bucket+prior.resolution {
							contained = true
							break
						}
					}
					if !contained {
						outside = append(outside, c)
					}
				}
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
			if !alignTime(time.Unix(0, first).UTC(), q.Interval).Equal(alignTime(time.Unix(0, last).UTC(), q.Interval)) {
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
						return fmt.Errorf("%w: non-nested source representations", ErrTelemetryCoverage)
					}
				}
				if covered {
					continue
				}
			}
			chosen = append(chosen, c)
			k, err := telemetryPlanGroup(ctx, q, c.name, c.entity, c.tags, first)
			if err != nil {
				return err
			}
			if card {
				if !ownerAllocated {
					if err = reserveChartMemory(ctx, 0, 512); err != nil {
						return err
					}
					ownerAllocated = true
				}
				// Use card precision in both stages. Recompressing each owner's
				// population at the smaller storage precision before the final
				// merge compounds quantile error on real mixed-boundary windows.
				if err = mergeChartStats(ctx, &ownerStats, c.stats); err != nil {
					return err
				}
				ownerKey, ownerTags = k, c.tags
			} else {
				if groups[k] == nil {
					if err = reserveChartMemory(ctx, 1, 512); err != nil {
						return err
					}
					groups[k] = &telemetryStats{}
					tagsByGroup[k] = c.tags
				}
				if err = mergeChartStats(ctx, groups[k], c.stats); err != nil {
					return err
				}
			}
			p.selected[owner] = append(p.selected[owner], telemetryPlanRange{c.bucket, c.bucket + c.resolution, c.through})
			out.Resolution = max(out.Resolution, time.Duration(c.resolution))
			sources = append(sources, TelemetrySourceSpan{MetricName: c.name, EntityID: c.entity, Tags: c.tags, Start: time.Unix(0, max(c.bucket, q.Start.UnixNano())).UTC(), End: time.Unix(0, min(c.bucket+c.resolution, q.End.UnixNano()+1)).UTC(), Resolution: time.Duration(c.resolution)})
		}
		// A coarse parent can straddle both the raw cutoff and the requested
		// edge. Its historical children may be fully represented even though
		// one recent child needs exact hot SQL. Count those disjoint recent
		// children as coverage proof only: do not select their coarse stats or
		// exclude their raw observations from the query below.
		var exactHot []telemetryPlanCell
		if chart {
			for _, c := range cells {
				if c.state == "legacy" || c.stats.Attempts == 0 || c.stats.FirstNano < cutoff {
					continue
				}
				overlaps := false
				for _, proof := range [][]telemetryPlanCell{chosen, outside, exactHot} {
					for _, prior := range proof {
						if c.bucket < prior.bucket+prior.resolution && c.bucket+c.resolution > prior.bucket {
							overlaps = true
							break
						}
					}
					if overlaps {
						break
					}
				}
				if !overlaps {
					if err := reserveChartMemory(ctx, 0, 512); err != nil {
						return err
					}
					proofBytes += 512
					exactHot = append(exactHot, c)
				}
			}
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
			if c.state != "legacy" {
				for _, excluded := range outside {
					if excluded.bucket >= c.bucket && excluded.bucket+excluded.resolution <= c.bucket+c.resolution {
						represented += excluded.stats.Attempts
					}
				}
				for _, recent := range exactHot {
					if recent.bucket >= c.bucket && recent.bucket+recent.resolution <= c.bucket+c.resolution {
						represented += recent.stats.Attempts
					}
				}
			}
			// For alternate online tiers, equal represented counts prove that no
			// source observation remains, including known outside observations and
			// legitimate empty fine buckets. Excluded cells never enter the result.
			if covered || c.state != "legacy" && represented == c.stats.Attempts {
				continue
			}
			from, through := max(c.stats.FirstNano, q.Start.UnixNano()), min(c.stats.LastNano, q.End.UnixNano())
			if chart && from >= cutoff {
				continue
			} // bounded exact hot boundary below
			if !chart {
				return ErrTelemetryCoverage
			}
			through = min(through, cutoff-1)
			if through < from {
				continue
			}
			start := alignTime(time.Unix(0, from).UTC(), q.Interval)
			for at := start; at.UnixNano() <= through; at = at.Add(q.Interval) {
				k, err := telemetryPlanGroup(ctx, q, c.name, c.entity, c.tags, at.UnixNano())
				if err != nil {
					return err
				}
				if !p.missing[k] {
					if err := reserveChartMemory(ctx, 0, 256); err != nil {
						return err
					}
				}
				p.missing[k] = true
				if len(p.missing) > 100000 {
					return ErrTelemetryBackpressure
				}
			}
			gaps = append(gaps, TelemetrySourceSpan{MetricName: c.name, EntityID: c.entity, Tags: c.tags, Start: time.Unix(0, from).UTC(), End: time.Unix(0, through+1).UTC(), Resolution: time.Duration(c.resolution), Reason: "retained_precision_or_boundary"})
		}
		if ownerAllocated {
			if groups[ownerKey] == nil {
				if err := reserveChartMemory(ctx, 1, 512); err != nil {
					return err
				}
				groups[ownerKey] = &telemetryStats{}
				tagsByGroup[ownerKey] = ownerTags
			}
			if err := mergeChartStats(ctx, groups[ownerKey], ownerStats); err != nil {
				return err
			}
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

		// Keep only the union, not the source-sized backing array. Source cells
		// and proof slices are released after each ordered owner is processed.
		union = append([]telemetryPlanRange(nil), union...)
		compactSources := coalesceTelemetrySpans(sources)
		compactGaps := coalesceTelemetrySpans(gaps)
		retained := 256 + len(owner.segment) + len(owner.series) + len(union)*48
		for _, spans := range [][]TelemetrySourceSpan{compactSources, compactGaps} {
			for _, span := range spans {
				encoded, _ := json.Marshal(span.Tags)
				retained += 512 + len(span.MetricName) + len(span.EntityID) + 4*len(encoded)
			}
		}
		if err := reserveChartMemory(ctx, 0, retained); err != nil {
			return err
		}
		p.selected[owner] = union
		if len(cells) > 0 && cells[0].state == "legacy" {
			p.legacy[owner.series] = append(p.legacy[owner.series], union...)
		}
		out.Sources = append(out.Sources, compactSources...)
		out.Gaps = append(out.Gaps, compactGaps...)
		return nil
	}
	query := "SELECT b.segment_id,b.series_id,a.state,a.last_id,s.metric,s.entity,s.tags,b.bucket,b.resolution,b.stats FROM " + t.table("summary") + " b JOIN " + t.table("members") + " m ON m.segment_id=b.segment_id AND m.series_id=b.series_id JOIN " + t.table("series") + " s ON s.id=m.series_id JOIN " + t.table("segments") + " a ON a.id=b.segment_id LEFT JOIN " + t.table("erasures") + " e ON e.series_id=s.id WHERE s.metric=? AND m.first_observed<? AND m.last_observed>=? AND ((a.state='legacy' AND b.bucket<=? AND b.bucket+b.resolution>?) OR (a.state<>'legacy' AND b.bucket<? AND b.bucket+b.resolution>?)) AND (e.through_id IS NULL OR a.first_id>e.through_id)" + filter + " ORDER BY b.segment_id,b.series_id,b.resolution DESC,b.bucket LIMIT 100001"
	args = append([]any{q.MetricName, boundaryEnd, boundaryStart, q.End.UnixNano(), q.Start.UnixNano(), boundaryEnd, boundaryStart}, args...)
	rows, err := tx.QueryContext(ctx, t.bind(query), args...)
	if err != nil {
		return nil, err
	}
	// The cursor is ordered by owner. Retain one owner's alternate tiers,
	// rather than materializing every retained fine cell for the whole query.
	var current telemetryPlanOwner
	var cells []telemetryPlanCell
	cellBytes := 0
	flushOwner := func() error {
		if len(cells) == 0 {
			return nil
		}
		err := processOwner(current, cells)
		cells = nil
		releaseChartMemory(ctx, cellBytes)
		cellBytes = 0
		return err
	}
	count, bytes := 0, 0
	for rows.Next() {
		var c telemetryPlanCell
		var tags, raw string
		if err = rows.Scan(&c.segment, &c.series, &c.state, &c.through, &c.name, &c.entity, &tags, &c.bucket, &c.resolution, &raw); err != nil {
			break
		}
		owner := telemetryPlanOwner{c.segment, c.series}
		if len(cells) > 0 && owner != current {
			if err = flushOwner(); err != nil {
				break
			}
		}
		current = owner
		count++
		// Bound actual SQL input bytes separately from resident cell metadata,
		// which is reserved below and refunded after each owner is processed.
		bytes += len(raw) + len(tags)
		baseBytes := len(c.segment) + len(c.series) + len(c.name) + len(c.entity) + 4*len(tags) + 1024
		if err = reserveChartMemory(ctx, 0, baseBytes); err != nil {
			break
		}
		if count > 100000 || bytes > 32<<20 {
			err = ErrTelemetryBackpressure
			break
		}
		if c.resolution <= 0 || c.bucket+c.resolution <= c.bucket {
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
			releaseChartMemory(ctx, baseBytes)
			continue
		}
		if err = decodeChartStats(ctx, raw, &c.stats); err != nil {
			break
		}
		cells = append(cells, c)
		cellBytes += baseBytes + chartStatsBytes(&c.stats)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	if err = flushOwner(); err != nil {
		return nil, err
	}
	return p, nil
}

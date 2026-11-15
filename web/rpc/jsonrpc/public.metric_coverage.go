package jsonrpc

import (
	"github.com/komari-monitor/komari/pkg/metric"
	"sort"
	"time"
)

type publicMetricSourceSpan struct {
	Start                 time.Time `json:"start"`
	End                   time.Time `json:"end"`
	SourceIntervalSeconds float64   `json:"source_interval_seconds"`
	Reason                string    `json:"reason"`
}

// Project coverage before filling empties. A gap-only tag series must survive
// splitting, and an unavailable bucket must never become an ordinary null.
func publicTelemetryMetricSeries(base publicMetricSeries, r metric.TelemetryQueryResult) []publicMetricSeries {
	byTags := map[string]*publicMetricSeries{}
	get := func(tags map[string]string) *publicMetricSeries {
		key := publicMetricTagsKey(tags)
		if byTags[key] == nil {
			v := base
			v.Tags = tags
			v.Points = []publicMetricPoint{}
			v.Sources = nil
			v.Gaps = nil
			byTags[key] = &v
		}
		return byTags[key]
	}
	for _, p := range base.Points {
		s := get(p.Tags)
		s.Points = append(s.Points, p)
	}
	for _, pair := range []struct {
		spans []metric.TelemetrySourceSpan
		gap   bool
	}{{r.Sources, false}, {r.Gaps, true}} {
		for _, sp := range pair.spans {
			if sp.EntityID != base.EntityID {
				continue
			}
			s := get(sp.Tags)
			v := publicMetricSourceSpan{sp.Start, sp.End, sp.Resolution.Seconds(), sp.Reason}
			if pair.gap {
				s.Gaps = append(s.Gaps, v)
			} else {
				s.Sources = append(s.Sources, v)
			}
		}
	}
	if len(byTags) == 0 {
		get(base.Tags)
	}
	keys := make([]string, 0, len(byTags))
	for key := range byTags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]publicMetricSeries, 0, len(keys))
	for _, key := range keys {
		s := *byTags[key]
		start, end := r.CoverageStart, r.CoverageEnd
		if base.CoverageStart != nil && base.CoverageStart.After(start) {
			start = *base.CoverageStart
		}
		if base.CoverageEnd != nil && base.CoverageEnd.Before(end) {
			end = *base.CoverageEnd
		}
		s.CoverageStart, s.CoverageEnd = &start, &end
		s.BoundaryAdjusted = s.BoundaryAdjusted || r.BoundaryAdjusted
		s.AcceptedThrough, s.SummarizedThrough = r.AcceptedThrough, r.AccountedThrough
		s.RequestedIntervalSeconds = base.IntervalSeconds
		interval := time.Duration(base.IntervalSeconds * float64(time.Second))
		if s.FillEmpty {
			s = adaptiveFillPublicMetricSeries(s, start, end)
		}
		points := map[int64]publicMetricPoint{}
		for _, p := range s.Points {
			p.Coverage = "complete"
			if p.Value == nil {
				p.Coverage = "empty"
			}
			for _, sp := range s.Sources {
				if sp.Start.Before(p.Time.Add(interval)) && sp.End.After(p.Time) {
					p.SourceIntervalSeconds = max(p.SourceIntervalSeconds, sp.SourceIntervalSeconds)
				}
			}
			points[p.Time.UnixNano()] = p
		}
		for _, gap := range s.Gaps {
			for at := metric.TelemetryBucketStart(gap.Start, interval); at.Before(gap.End); at = at.Add(interval) {
				points[at.UnixNano()] = publicMetricPoint{Time: at, Tags: s.Tags, Coverage: "unavailable", CoverageReason: gap.Reason, SourceIntervalSeconds: gap.SourceIntervalSeconds}
			}
		}
		s.Points = make([]publicMetricPoint, 0, len(points))
		present := 0
		for _, p := range points {
			s.Points = append(s.Points, p)
			if p.Value != nil {
				present++
			}
		}
		sort.Slice(s.Points, func(i, j int) bool { return s.Points[i].Time.Before(s.Points[j].Time) })
		s.Coverage = "complete"
		if len(s.Gaps) > 0 {
			s.Coverage = "partial"
			if present == 0 {
				s.Coverage = "unavailable"
			}
		}
		s.Count = len(s.Points)
		out = append(out, s)
	}
	return out
}

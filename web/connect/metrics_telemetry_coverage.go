package connectapi

import (
	"github.com/komari-monitor/komari/pkg/metric"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sort"
	"time"
)

func connectTelemetryAggregateSeries(name, id string, definition metric.Definition, aggregation metric.Aggregation, interval time.Duration, fill bool, start, end time.Time, result metric.TelemetryQueryResult, indices []int) []*metricsv1.MetricsSeries {
	series := connectAggregateSeries(name, id, definition, aggregation, interval, fill, start, end, result.Points, indices)
	byTags := map[string]*metricsv1.MetricsSeries{}
	get := func(tags map[string]string) *metricsv1.MetricsSeries {
		key := metricTagsKey(tags)
		if byTags[key] == nil {
			byTags[key] = &metricsv1.MetricsSeries{AgentId: id, Metric: name, Labels: cloneLabels(tags), Type: string(definition.Type), Unit: definition.Unit, RetentionDays: uint32(max(definition.RetentionDays, 0)), Downsampled: true, Aggregation: string(aggregation), Interval: durationpb.New(interval)}
		}
		return byTags[key]
	}
	for _, s := range series {
		byTags[metricTagsKey(s.Labels)] = s
	}
	spanProto := func(s metric.TelemetrySourceSpan) *metricsv1.MetricsSourceSpan {
		return &metricsv1.MetricsSourceSpan{StartTime: timestamppb.New(s.Start), EndTime: timestamppb.New(s.End), SourceInterval: durationpb.New(s.Resolution), Reason: s.Reason}
	}
	for _, span := range result.Sources {
		if span.EntityID != id {
			continue
		}
		s := get(span.Tags)
		s.Sources = append(s.Sources, spanProto(span))
	}
	for _, span := range result.Gaps {
		if span.EntityID != id {
			continue
		}
		s := get(span.Tags)
		s.Gaps = append(s.Gaps, spanProto(span))
	}
	if len(byTags) == 0 {
		get(nil)
	}
	out := make([]*metricsv1.MetricsSeries, 0, len(byTags))
	for _, s := range byTags {
		s.RequestedInterval = durationpb.New(interval)
		s.Coverage = "complete"
		s.CoverageStart = timestamppb.New(start)
		s.CoverageEnd = timestamppb.New(end)
		s.SummarizedThrough = uint64(max(result.AccountedThrough, 0))
		s.AcceptedThrough = uint64(max(result.AcceptedThrough, 0))
		points := map[int64]*metricsv1.QueryPoint{}
		for _, p := range s.QueryPoints {
			at := p.ObservedAt.AsTime()
			p.Coverage = "complete"
			var resolution time.Duration
			for _, span := range s.Sources {
				if span.StartTime.AsTime().Before(at.Add(interval)) && span.EndTime.AsTime().After(at) {
					resolution = max(resolution, span.SourceInterval.AsDuration())
				}
			}
			p.SourceInterval = durationpb.New(resolution)
			points[at.UnixNano()] = p
		}
		for _, span := range s.Gaps {
			s.BoundaryAdjusted = true
			for at := span.StartTime.AsTime().UTC().Truncate(interval); at.Before(span.EndTime.AsTime()); at = at.Add(interval) {
				points[at.UnixNano()] = &metricsv1.QueryPoint{ObservedAt: timestamppb.New(at), SourceInterval: span.SourceInterval, Coverage: "unavailable", CoverageReason: span.Reason}
			}
		}
		s.QueryPoints = nil
		present := 0
		for _, p := range points {
			s.QueryPoints = append(s.QueryPoints, p)
			if p.Value != nil {
				present++
			}
		}
		sort.Slice(s.QueryPoints, func(i, j int) bool {
			return s.QueryPoints[i].ObservedAt.AsTime().Before(s.QueryPoints[j].ObservedAt.AsTime())
		})
		if len(s.Gaps) > 0 {
			s.Coverage = "partial"
			if present == 0 {
				s.Coverage = "unavailable"
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return metricTagsKey(out[i].Labels) < metricTagsKey(out[j].Labels) })
	return out
}

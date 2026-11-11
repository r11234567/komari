package connectapi

import (
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

func TestTelemetryCoveragePreservesZeroAndIsolatesEntitiesAndTags(t *testing.T) {
	at := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	result := metric.TelemetryQueryResult{
		AccountedThrough: 5, AcceptedThrough: 7,
		Points:  []metric.AggregatePoint{{EntityID: "node", MetricName: "cpu", Bucket: at, Value: 0, Count: 1, Tags: map[string]string{"device": "0"}}},
		Sources: []metric.TelemetrySourceSpan{{MetricName: "cpu", EntityID: "node", Tags: map[string]string{"device": "0"}, Start: at, End: at.Add(time.Minute), Resolution: time.Minute}},
		Gaps: []metric.TelemetrySourceSpan{
			{MetricName: "cpu", EntityID: "node", Tags: map[string]string{"device": "1"}, Start: at, End: at.Add(time.Minute), Resolution: time.Hour, Reason: "precision"},
			{MetricName: "cpu", EntityID: "other", Tags: map[string]string{"device": "0"}, Start: at, End: at.Add(time.Minute), Reason: "other"},
		},
	}
	got := connectTelemetryAggregateSeries("cpu", "node", metric.Definition{}, metric.AggAvg, time.Minute, false, at, at.Add(2*time.Minute), result, []int{0})
	if len(got) != 2 {
		t.Fatal(got)
	}
	for _, s := range got {
		if s.SummarizedThrough != 5 || s.AcceptedThrough != 7 || s.RequestedInterval.AsDuration() != time.Minute {
			t.Fatalf("lost metadata: %+v", s)
		}
		if s.Labels["device"] == "0" {
			if s.Coverage != "complete" || len(s.Gaps) != 0 || len(s.QueryPoints) != 1 || s.QueryPoints[0].Value == nil || *s.QueryPoints[0].Value != 0 || s.QueryPoints[0].SourceInterval.AsDuration() != time.Minute {
				t.Fatalf("zero or isolation: %+v", s)
			}
		} else if s.Coverage != "unavailable" || len(s.QueryPoints) != 1 || s.QueryPoints[0].Value != nil || s.QueryPoints[0].CoverageReason != "precision" {
			t.Fatalf("unknown became a number: %+v", s)
		}
	}
	// An unavailable contribution replaces the numeric result for the same
	// bucket even if another segment provided an otherwise valid subtotal.
	result.Gaps[0].Tags = map[string]string{"device": "0"}
	got = connectTelemetryAggregateSeries("cpu", "node", metric.Definition{}, metric.AggAvg, time.Minute, false, at, at.Add(2*time.Minute), result, []int{0})
	if len(got) != 1 || got[0].QueryPoints[0].Value != nil || got[0].Coverage != "unavailable" {
		t.Fatal(got)
	}
}

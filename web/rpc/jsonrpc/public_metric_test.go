package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestPublicMetricsBatchesByMetricAndPreservesEntityScope(t *testing.T) {
	for _, telemetry := range []bool{true, false} {
		t.Run(fmt.Sprint("telemetry=", telemetry), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s, err := metric.Open(ctx, metric.SQLite(filepath.Join(dir, "metrics.db"), metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if telemetry {
				if err = s.EnableTelemetry(ctx, metric.TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")}); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC().Truncate(10 * time.Second)
			at := now.Add(-2*time.Minute + time.Second)
			var keys, visible []string
			for n := 0; n < 6; n++ {
				visible = append(visible, fmt.Sprintf("node-%d", n))
			}
			for m := 0; m < 12; m++ {
				key := fmt.Sprintf("audit.metric.%02d", m)
				keys = append(keys, key)
				if err = s.CreateMetric(ctx, metric.Definition{Name: key, RetentionDays: 30}); err != nil {
					t.Fatal(err)
				}
				for n, entity := range append(append([]string{}, visible...), "hidden", "unrelated") {
					var points []metric.Point
					for tag := 0; tag < 2; tag++ {
						for sample := 0; sample < 2; sample++ {
							points = append(points, metric.Point{MetricName: key, EntityID: entity, Timestamp: at.Add(time.Duration(sample) * time.Second), Value: float64(m*1000 + n*100 + tag*10 + sample*2), Tags: map[string]string{"device": fmt.Sprint(tag)}, Labels: map[string]string{"source": "synthetic"}})
						}
					}
					if err = s.WriteBatch(ctx, points); err != nil {
						t.Fatal(err)
					}
				}
			}
			if telemetry {
				for {
					n, err := s.Telemetry().SummarizeNext(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if n == 0 {
						break
					}
				}
			}
			for _, downsample := range []bool{true, false} {
				for _, tc := range []struct {
					name string
					ids  []string
					tags map[string]string
					fill bool
				}{
					{"all_visible", visible, nil, false},
					{"explicit_node_and_tag", visible[2:3], map[string]string{"device": "1"}, false},
					{"empty_authorized_set", []string{}, nil, true},
					{"no_data", []string{"unknown"}, nil, false},
				} {
					t.Run(fmt.Sprintf("%s/downsample=%t", tc.name, downsample), func(t *testing.T) {
						params := publicMetricQueryParams{Downsample: &downsample, FillEmpty: &tc.fill, MaxPoints: 600, Tags: tc.tags}
						series, rpcErr := queryPublicMetricSeries(ctx, s, params, keys, tc.ids, now.Add(-5*time.Minute), now, now)
						if rpcErr != nil {
							t.Fatal(rpcErr)
						}
						if tc.name == "no_data" {
							if len(series) != len(keys) {
								t.Fatal("missing empty series", len(series))
							}
							for _, item := range series {
								if item.EntityID != "unknown" || item.Points == nil || len(item.Points) != 0 {
									t.Fatal("empty series contract", item)
								}
							}
							return
						}
						perEntity := 2
						if len(tc.tags) > 0 {
							perEntity = 1
						}
						if len(series) != len(keys)*len(tc.ids)*perEntity {
							t.Fatal("entity/tag series lost", len(series))
						}
						for i, item := range series {
							m := i / (len(tc.ids) * perEntity)
							entity := tc.ids[(i/perEntity)%len(tc.ids)]
							var n, tag int
							fmt.Sscanf(entity, "node-%d", &n)
							fmt.Sscanf(item.Tags["device"], "%d", &tag)
							if item.EntityID != entity || item.MetricKey != keys[m] || item.Downsampled != downsample {
								t.Fatal("response order or identity", item)
							}
							wantLen := 2
							if downsample && telemetry {
								wantLen = 1
							}
							if len(item.Points) != wantLen {
								t.Fatal("population", item)
							}
							for sample, p := range item.Points {
								want := float64(m*1000 + n*100 + tag*10 + sample*2)
								if downsample && telemetry {
									want++
								}
								if p.Value == nil || *p.Value != want || p.Tags["device"] != item.Tags["device"] {
									t.Fatal("cross-entity data or wrong mean", item)
								}
								if downsample && p.Count != map[bool]int{true: 2, false: 1}[telemetry] {
									t.Fatal("aggregate population", p)
								}
								if !downsample && (p.Labels["source"] != "synthetic" || !p.Time.Equal(at.Add(time.Duration(sample)*time.Second))) {
									t.Fatal("raw timestamp/labels changed", p)
								}
							}
						}
					})
				}
			}
		})
	}
}

func TestPublicTelemetryCoverageSurvivesTagSplitAndEmptyFill(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	v := float64(12)
	base := publicMetricSeries{MetricKey: "cpu", EntityID: "node", Downsampled: true, IntervalSeconds: 60, FillEmpty: true, Points: []publicMetricPoint{{Time: at, Value: &v, Tags: map[string]string{"device": "a"}}}}
	r := metric.TelemetryQueryResult{CoverageStart: at, CoverageEnd: at.Add(3*time.Minute - time.Nanosecond), BoundaryAdjusted: true, AcceptedThrough: 9, AccountedThrough: 8, Sources: []metric.TelemetrySourceSpan{{MetricName: "cpu", EntityID: "node", Tags: map[string]string{"device": "a"}, Start: at, End: at.Add(time.Minute), Resolution: 10 * time.Second}}, Gaps: []metric.TelemetrySourceSpan{{MetricName: "cpu", EntityID: "node", Tags: map[string]string{"device": "a"}, Start: at.Add(time.Minute), End: at.Add(2 * time.Minute), Reason: "missing raw"}, {MetricName: "cpu", EntityID: "node", Tags: map[string]string{"device": "b"}, Start: at, End: at.Add(time.Minute), Reason: "crossed coarse cell"}, {MetricName: "cpu", EntityID: "hidden", Tags: map[string]string{"device": "secret"}, Start: at, End: at.Add(time.Minute), Reason: "hidden"}}}
	series := publicTelemetryMetricSeries(base, r)
	if len(series) != 2 {
		t.Fatal("lost gap-only tag or leaked hidden series", series)
	}
	for _, s := range series {
		if s.AcceptedThrough != 9 || s.SummarizedThrough != 8 || !s.BoundaryAdjusted || len(s.Gaps) != 1 {
			t.Fatal("metadata lost", s)
		}
		want := "partial"
		if s.Tags["device"] == "b" {
			want = "unavailable"
		}
		if s.Coverage != want {
			t.Fatal("coverage collapsed", s)
		}
		missing := 0
		for _, p := range s.Points {
			if p.Coverage == "unavailable" {
				missing++
				if p.Value != nil || p.CoverageReason == "" {
					t.Fatal("gap became empty/valid", p)
				}
			}
		}
		if missing != 1 {
			t.Fatal("gap marker missing", s)
		}
	}
	b, err := json.Marshal(series)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Resolution") || !strings.Contains(string(b), "source_interval_seconds") {
		t.Fatal("internal source DTO leaked", string(b))
	}
}

func TestMetricQueryParamsRequireRFC3339Time(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		wantErr bool
	}{
		{name: "RFC3339", value: "2026-07-17T09:30:00.123456789+08:00"},
		{name: "offset free", value: "2026-07-17 09:30:00.123456789", wantErr: true},
		{name: "Unix number", value: float64(1_752_720_600), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := &rpc.JsonRpcRequest{Params: map[string]any{"start": test.value}}
			var params publicMetricQueryParams
			err := req.BindParams(&params)
			if (err != nil) != test.wantErr {
				t.Fatalf("BindParams() error = %v, wantErr %v", err, test.wantErr)
			}
			if err == nil {
				want := time.Date(2026, 7, 17, 1, 30, 0, 123456789, time.UTC)
				if params.Start == nil || !params.Start.Equal(want) {
					t.Fatalf("start = %v, want %s", params.Start, want)
				}
			}
		})
	}
}

func TestSplitPublicMetricSeriesKeepsTagSeries(t *testing.T) {
	baseTime := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	base := publicMetricSeries{
		MetricKey: "gpu.device.usage",
		EntityID:  "node-a",
		Points: []publicMetricPoint{
			{Time: baseTime, Value: publicMetricValue(10), Count: 2, Tags: map[string]string{"device_index": "0"}},
			{Time: baseTime, Value: publicMetricValue(80), Count: 2, Tags: map[string]string{"device_index": "1"}},
			{Time: baseTime.Add(time.Minute), Value: publicMetricValue(20), Count: 2, Tags: map[string]string{"device_index": "0"}},
		},
	}

	got := splitPublicMetricSeries(base)
	if len(got) != 2 {
		t.Fatalf("expected 2 tag series, got %d: %#v", len(got), got)
	}
	if got[0].Tags["device_index"] != "0" || got[0].Count != 2 {
		t.Fatalf("unexpected first series: %#v", got[0])
	}
	if got[1].Tags["device_index"] != "1" || got[1].Count != 1 {
		t.Fatalf("unexpected second series: %#v", got[1])
	}
	if got[0].Points[0].Tags["device_index"] != "0" || got[1].Points[0].Tags["device_index"] != "1" {
		t.Fatalf("point tags were not preserved: %#v", got)
	}
}

func TestPublicMetricJSONIncludesOnlyTags(t *testing.T) {
	pointTime := time.Date(2026, 6, 18, 0, 0, 0, 123456789, time.UTC)
	payload, err := json.Marshal(publicMetricSeries{
		MetricKey: "ping.loss",
		EntityID:  "node-a",
		Tags:      map[string]string{"task_id": "7"},
		Points: []publicMetricPoint{{
			Time:  pointTime,
			Value: publicMetricValue(0),
			Tags:  map[string]string{"task_id": "7"},
		}},
	})
	if err != nil {
		t.Fatalf("marshal series: %v", err)
	}
	text := string(payload)
	if !strings.Contains(text, `"tags":{"task_id":"7"}`) {
		t.Fatalf("series tags missing: %s", text)
	}
	if strings.Contains(text, `"tag":`) {
		t.Fatalf("legacy tag field should not be serialized: %s", text)
	}
	if !strings.Contains(text, `"time":"2026-06-18T00:00:00.123456789Z"`) {
		t.Fatalf("metric time is not UTC RFC3339Nano: %s", text)
	}
}

func TestAdaptiveFillPublicMetricSeriesUsesObservedInterval(t *testing.T) {
	base := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	series := publicMetricSeries{
		MetricKey:       "cpu.usage",
		EntityID:        "node-a",
		FillEmpty:       true,
		IntervalSeconds: 1,
		Tags:            map[string]string{"core": "0"},
	}
	for i := 0; i < 10; i++ {
		series.Points = append(series.Points, publicMetricPoint{
			Time:  base.Add(time.Duration(i) * time.Minute),
			Value: publicMetricValue(float64(i)),
		})
	}

	got := adaptiveFillPublicMetricSeries(series, base, base.Add(9*time.Minute))
	if got.IntervalSeconds != 60 {
		t.Fatalf("expected observed 60s interval, got %v", got.IntervalSeconds)
	}
	if len(got.Points) != 10 {
		t.Fatalf("regular sparse series should not gain null buckets, got %#v", got.Points)
	}
	for _, point := range got.Points {
		if point.Value == nil {
			t.Fatalf("regular sparse series gained a null point: %#v", got.Points)
		}
	}
}

func TestAdaptiveFillPublicMetricSeriesAddsCompactGapsAndBounds(t *testing.T) {
	base := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	tags := map[string]string{"device_index": "0"}
	series := publicMetricSeries{
		MetricKey:       "gpu.device.usage",
		EntityID:        "node-a",
		IntervalSeconds: 1,
		Tags:            tags,
		Points: []publicMetricPoint{
			{Time: base, Value: publicMetricValue(10)},
			{Time: base.Add(time.Minute), Value: publicMetricValue(20)},
			{Time: base.Add(2 * time.Minute), Value: publicMetricValue(30)},
			{Time: base.Add(4 * time.Minute), Value: publicMetricValue(40)},
		},
	}
	start := base.Add(-30 * time.Second)
	end := base.Add(4*time.Minute + 30*time.Second)
	got := adaptiveFillPublicMetricSeries(series, start, end)
	if got.IntervalSeconds != 60 {
		t.Fatalf("expected observed 60s interval, got %v", got.IntervalSeconds)
	}
	if len(got.Points) != 7 {
		t.Fatalf("expected four values, one gap and two bounds, got %#v", got.Points)
	}
	wantNullTimes := map[time.Time]bool{
		start:                     true,
		base.Add(3 * time.Minute): true,
		end:                       true,
	}
	for _, point := range got.Points {
		if point.Value != nil {
			continue
		}
		if !wantNullTimes[point.Time] {
			t.Fatalf("unexpected null point at %s: %#v", point.Time, got.Points)
		}
		if point.Tags["device_index"] != "0" {
			t.Fatalf("adaptive null point lost tags: %#v", point)
		}
		delete(wantNullTimes, point.Time)
	}
	if len(wantNullTimes) != 0 {
		t.Fatalf("missing expected null points: %#v", wantNullTimes)
	}
}

func TestPublicPingMetricFillEmptyMapsMinusOneToNull(t *testing.T) {
	for _, metricName := range []string{metricstore.MetricPingLatency, metricstore.MetricPingLoss} {
		if value := publicRawMetricValue(metricName, -1, true); value != nil {
			t.Fatalf("raw %s -1 should become null when fill_empty is enabled, got %v", metricName, *value)
		}
	}
	if value := publicRawMetricValue(metricstore.MetricPingLatency, -1, true); value != nil {
		t.Fatalf("downsampled ping -1 should become null when fill_empty is enabled, got %v", *value)
	}
}

func TestPublicPingMetricMinusOneIsPreservedWithoutFillEmpty(t *testing.T) {
	value := publicRawMetricValue(metricstore.MetricPingLatency, -1, false)
	if value == nil || *value != -1 {
		t.Fatalf("raw ping -1 should be preserved when fill_empty is disabled, got %v", value)
	}

	nonPing := publicRawMetricValue("temperature", -1, true)
	if nonPing == nil || *nonPing != -1 {
		t.Fatalf("negative values from non-ping metrics must be preserved, got %v", nonPing)
	}
}

func TestPublicPingStatsFromAggregateGroupsUsesTaskNamesAndLossMetric(t *testing.T) {
	base := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	taskMap := map[string]models.PingTask{
		"1": {Id: 1, Name: "Tokyo ICMP", Type: "icmp", Interval: 60},
	}
	groups := publicPingMetricAggregateGroups{
		Avg: map[string][]metric.AggregatePoint{
			"1": {
				{Bucket: base, Count: 2, Value: 20},
				{Bucket: base.Add(time.Minute), Count: 2, Value: 40},
			},
		},
		Min: map[string][]metric.AggregatePoint{
			"1": {{Bucket: base, Count: 4, Value: 12}},
		},
		Max: map[string][]metric.AggregatePoint{
			"1": {{Bucket: base, Count: 4, Value: 92}},
		},
		Last: map[string][]metric.AggregatePoint{
			"1": {{Bucket: base.Add(time.Minute), Count: 1, Value: 44}},
		},
		P50: map[string][]metric.AggregatePoint{
			"1": {{Bucket: base, Count: 4, Value: 30}},
		},
		P99: map[string][]metric.AggregatePoint{
			"1": {{Bucket: base, Count: 4, Value: 80}},
		},
		StdDev: map[string][]metric.AggregatePoint{
			"1": {{Bucket: base, Count: 4, Value: 8}},
		},
		Loss: map[string][]metric.AggregatePoint{
			"1": {{Bucket: base, Count: 4, Value: 0.25}},
		},
	}

	stats := publicPingStatsFromAggregateGroups("node-a", groups, taskMap, nil)
	if len(stats) != 1 {
		t.Fatalf("expected one stat, got %#v", stats)
	}
	got := stats[0]
	if got.Name != "Tokyo ICMP" || got.Type != "icmp" || got.Interval != 60 {
		t.Fatalf("task metadata not applied: %#v", got)
	}
	if got.Total != 4 || got.Valid != 3 {
		t.Fatalf("unexpected totals: %#v", got)
	}
	if got.Loss != 25 || got.LossApproximate {
		t.Fatalf("loss should come from ping.loss metric: %#v", got)
	}
	if got.Min == nil || *got.Min != 12 || got.Max == nil || *got.Max != 92 || got.Avg == nil || *got.Avg != 30 {
		t.Fatalf("latency stats mismatch: %#v", got)
	}
	if math.Abs(got.P99P50Ratio-1.6666666666666667) > 0.000001 {
		t.Fatalf("unexpected volatility ratio: %#v", got)
	}
}

func TestPublicPingMetricStatsIncludesZeroVolatility(t *testing.T) {
	payload, err := json.Marshal(publicPingMetricTaskStats{
		EntityID:    "node-a",
		TaskID:      "1",
		Total:       1,
		Valid:       1,
		P99P50Ratio: 0,
	})
	if err != nil {
		t.Fatalf("marshal ping stats: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal ping stats: %v", err)
	}
	if value, ok := decoded["p99_p50_ratio"]; !ok || value != float64(0) {
		t.Fatalf("zero volatility must be present, got %s", payload)
	}
}

func TestPublicPingStatisticsUseAttemptPopulationPerTask(t *testing.T) {
	// Scalar latency counts only successes; loss counts every attempt. Other
	// tasks having a loss series cannot establish coverage for this task.
	summary := metric.SeriesSummary{
		Avg: []metric.AggregatePoint{
			{Tags: map[string]string{"task_id": "1"}, Count: 2, Value: 20},
			{Tags: map[string]string{"task_id": "3"}, Count: 1, Value: 10},
			{Tags: map[string]string{"task_id": "3"}, Count: 1, Value: -1},
		},
		Loss: []metric.AggregatePoint{
			{Tags: map[string]string{"task_id": "1"}, Count: 3, Value: 1.0 / 3},
			{Tags: map[string]string{"task_id": "2"}, Count: 3, Value: 1},
		},
	}
	stats := publicPingStatsFromAggregateGroups("node", publicPingMetricGroupsFromSummary(summary), nil, nil)
	if len(stats) != 3 {
		t.Fatal(stats)
	}
	for _, stat := range stats {
		switch stat.TaskID {
		case "1":
			if stat.Total != 3 || stat.Valid != 2 || math.Abs(stat.Loss-100.0/3) > 1e-9 || stat.LossApproximate || stat.Avg == nil || *stat.Avg != 20 {
				t.Fatal(stat)
			}
		case "2":
			if stat.Total != 3 || stat.Valid != 0 || stat.Loss != 100 || stat.LossApproximate || stat.Avg != nil {
				t.Fatal(stat)
			}
		case "3":
			if stat.Total != 2 || stat.Valid != 1 || stat.Loss != 50 || !stat.LossApproximate || stat.Avg == nil || *stat.Avg != 10 {
				t.Fatal(stat)
			}
		}
	}
}

func TestMetricDownsampleIntervalCeilsToStandardInterval(t *testing.T) {
	got := metricDownsampleInterval(30*24*time.Hour, 500)
	if got != 2*time.Hour {
		t.Fatalf("30d/500 should ceil to 2h, got %s", got)
	}

	got = metricDownsampleInterval(time.Hour, 500)
	if got != 10*time.Second {
		t.Fatalf("1h/500 should ceil to 10s, got %s", got)
	}

	got = metricDownsampleInterval(1000*24*time.Hour, 10)
	if got != 100*24*time.Hour {
		t.Fatalf("ranges beyond the standard table should ceil to whole days, got %s", got)
	}
}

func TestResolveMetricMaxPointsRejectsOversizedQueries(t *testing.T) {
	if _, err := resolveMetricMaxPoints("cpu.usage", publicMetricQueryParams{MaxPoints: maxMetricQueryPoints + 1}); err == nil {
		t.Fatal("expected oversized max_points to be rejected")
	}
	got, err := resolveMetricMaxPoints("cpu.usage", publicMetricQueryParams{MaxPoints: maxMetricQueryPoints})
	if err != nil || got != maxMetricQueryPoints {
		t.Fatalf("max allowed points = %d, %v; want %d", got, err, maxMetricQueryPoints)
	}
}

func TestMetricQueryErrorMapsContextTermination(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code int
	}{
		{name: "deadline", err: context.DeadlineExceeded, code: rpc.DeadlineExceeded},
		{name: "cancelled", err: context.Canceled, code: rpc.Cancelled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := metricQueryError(errors.Join(errors.New("query failed"), test.err), "metrics")
			if got.Code != test.code {
				t.Fatalf("error code = %d, want %d", got.Code, test.code)
			}
		})
	}
}

package metricstore

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

type LegacyTrafficDelta struct {
	Observed time.Time
	Up, Down int64
}

// LegacyTrafficDeltas requires both byte counters and both explicitly recorded
// integer deltas at every retained observation. A partial/old population is
// left at its truthful hourly precision, never interpolated into minutes.
func LegacyTrafficDeltas(ctx context.Context, client string, start, end time.Time) ([]LegacyTrafficDelta, bool, error) {
	s := GetStore()
	if s == nil {
		return nil, false, ErrStoreNotInitialized
	}
	type sample struct {
		at     time.Time
		values [4]int64
		mask   uint8
	}
	samples := map[int64]*sample{}
	names := []string{MetricNetTotalUp, MetricNetTotalDown, MetricTrafficUp, MetricTrafficDown}
	incomplete := errors.New("partial legacy traffic population")
	for i, name := range names {
		err := s.VisitLegacyRaw(ctx, name, client, start, end, 32768, func(points []metric.Point) error {
			for _, p := range points {
				if len(p.Tags) > 0 || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) || p.Value < 0 || p.Value >= math.Exp2(53) || math.Trunc(p.Value) != p.Value {
					return incomplete
				}
				ns := p.Timestamp.UnixNano()
				row := samples[ns]
				if row == nil {
					if len(samples) >= 32768 {
						return metric.ErrTelemetryBackpressure
					}
					row = &sample{at: p.Timestamp}
					samples[ns] = row
				}
				if row.mask&(1<<i) != 0 {
					return incomplete
				}
				row.mask |= 1 << i
				row.values[i] = int64(p.Value)
			}
			return nil
		})
		if errors.Is(err, incomplete) || errors.Is(err, metric.ErrTelemetryBackpressure) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
	out := make([]LegacyTrafficDelta, 0, len(samples))
	for _, row := range samples {
		if row.mask != 15 {
			return nil, false, nil
		}
		out = append(out, LegacyTrafficDelta{Observed: row.at, Up: row.values[2], Down: row.values[3]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Observed.Before(out[j].Observed) })
	return out, len(out) > 0, nil
}

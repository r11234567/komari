package dashboard

import (
	"context"
	"fmt"
	"time"

	"github.com/komari-monitor/komari/database/trafficledger"
)

const (
	DefaultTrafficTrendWindow   = 24 * time.Hour
	DefaultTrafficTrendInterval = 20 * time.Minute
	MinTrafficTrendInterval     = 10 * time.Minute
	MaxTrafficTrendInterval     = 30 * time.Minute
	maxTrafficTrendBuckets      = 2048
)

type TrafficTrendBucket struct {
	Start time.Time
	Up    int64
	Down  int64
}

func LoadTrafficTrend(ctx context.Context, clientIDs []string, now time.Time, window, interval time.Duration) ([]TrafficTrendBucket, error) {
	if window <= 0 {
		window = DefaultTrafficTrendWindow
	}
	if interval <= 0 {
		interval = DefaultTrafficTrendInterval
	}
	if interval < MinTrafficTrendInterval || interval > MaxTrafficTrendInterval {
		return nil, fmt.Errorf("traffic trend interval must be between 10 and 30 minutes")
	}
	bucketCount := int((window + interval - 1) / interval)
	if bucketCount <= 0 || bucketCount > maxTrafficTrendBuckets {
		return nil, fmt.Errorf("traffic trend window produces an invalid number of buckets")
	}

	end := now.In(trafficledger.BeijingLocation).Truncate(interval).Add(interval)
	start := end.Add(-time.Duration(bucketCount) * interval)
	_, perClient, err := trafficledger.MetricUsageByIntervalBatch(ctx, clientIDs, start.UTC(), now.UTC(), interval)
	if err != nil {
		return nil, err
	}

	buckets := make([]TrafficTrendBucket, bucketCount)
	for index := range buckets {
		buckets[index].Start = start.Add(time.Duration(index) * interval)
	}
	for _, clientBuckets := range perClient {
		for _, bucket := range clientBuckets {
			index := int(bucket.Hour.In(trafficledger.BeijingLocation).Sub(start) / interval)
			if index < 0 || index >= len(buckets) {
				continue
			}
			buckets[index].Up += bucket.Up
			buckets[index].Down += bucket.Down
		}
	}
	return buckets, nil
}

package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/trafficledger"
	"golang.org/x/sync/singleflight"
)

const (
	DefaultTrafficTrendWindow   = 24 * time.Hour
	DefaultTrafficTrendInterval = 20 * time.Minute
	MinTrafficTrendInterval     = 10 * time.Minute
	MaxTrafficTrendInterval     = 30 * time.Minute
	maxTrafficTrendBuckets      = 2048
)

// ErrTrafficTrendBadRequest marks the failures that are genuinely caused by the
// caller's window/interval, so transport layers can tell them apart from a
// store that is merely busy.
var ErrTrafficTrendBadRequest = errors.New("invalid traffic trend request")

type TrafficTrendBucket struct {
	Start time.Time
	Up    int64
	Down  int64
}

// LoadTrafficTrend scans the traffic ledger for every client, which on SQLite
// takes the store's single heavy-read slot. Several dashboard panels ask for the
// same trend at once and every open tab repeats it, so the scan is shared: one
// caller runs it and the rest join, then a short cache absorbs the reload burst
// that follows. Without this the panels queue behind each other on one slot and
// time out together.
func LoadTrafficTrend(ctx context.Context, clientIDs []string, now time.Time, window, interval time.Duration) ([]TrafficTrendBucket, error) {
	if window <= 0 {
		window = DefaultTrafficTrendWindow
	}
	if interval <= 0 {
		interval = DefaultTrafficTrendInterval
	}
	if interval < MinTrafficTrendInterval || interval > MaxTrafficTrendInterval {
		return nil, fmt.Errorf("%w: interval must be between 10 and 30 minutes", ErrTrafficTrendBadRequest)
	}
	bucketCount := int((window + interval - 1) / interval)
	if bucketCount <= 0 || bucketCount > maxTrafficTrendBuckets {
		return nil, fmt.Errorf("%w: window produces an invalid number of buckets", ErrTrafficTrendBadRequest)
	}

	key := trafficTrendKey(clientIDs, now, window, interval)
	if cached, ok := cachedTrafficTrend(key, time.Now()); ok {
		return cached, nil
	}
	scan := trafficTrendFlight.DoChan(key, func() (any, error) {
		// The shared scan must not inherit one caller's deadline: whoever happens to
		// lead would otherwise cancel the scan for everyone waiting on it when its
		// own request times out or the browser navigates away. Give it a budget of
		// its own and let each caller apply its own deadline by selecting below.
		scanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), trafficTrendScanBudget)
		defer cancel()
		buckets, loadErr := loadTrafficTrendUncached(scanCtx, clientIDs, now, window, interval, bucketCount)
		if loadErr != nil {
			return nil, loadErr
		}
		storeTrafficTrend(key, buckets, time.Now())
		return buckets, nil
	})
	select {
	case result := <-scan:
		if result.Err != nil {
			return nil, result.Err
		}
		// Buckets are read-only once cached; hand every caller the same slice.
		return result.Val.([]TrafficTrendBucket), nil
	case <-ctx.Done():
		// Leave the shared scan running so it still populates the cache for the
		// callers that are waiting on it, and for the client's retry.
		return nil, ctx.Err()
	}
}

func loadTrafficTrendUncached(ctx context.Context, clientIDs []string, now time.Time, window, interval time.Duration, bucketCount int) ([]TrafficTrendBucket, error) {
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

const (
	// The shared scan outlives the caller that started it, so it needs a ceiling
	// of its own. Wider than any single caller's budget: a scan that is nearly
	// done should finish and fill the cache rather than be thrown away.
	trafficTrendScanBudget = 30 * time.Second
	trafficTrendCacheTTL   = 15 * time.Second
	// The window is quantized before it enters the key so that tabs computing
	// "now" a few milliseconds apart still share one scan.
	trafficTrendKeyQuantum = 30 * time.Second
	trafficTrendCacheLimit = 64
)

var trafficTrendFlight singleflight.Group

var trafficTrendCache struct {
	sync.Mutex
	items map[string]trafficTrendCacheEntry
}

type trafficTrendCacheEntry struct {
	expiresAt time.Time
	buckets   []TrafficTrendBucket
}

func trafficTrendKey(clientIDs []string, now time.Time, window, interval time.Duration) string {
	sorted := append([]string(nil), clientIDs...)
	sort.Strings(sorted)
	digest := sha256.New()
	fmt.Fprintf(digest, "%d\x00%d\x00%d\x00",
		now.UTC().Truncate(trafficTrendKeyQuantum).UnixNano(), window, interval)
	for _, id := range sorted {
		digest.Write([]byte(id))
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func cachedTrafficTrend(key string, at time.Time) ([]TrafficTrendBucket, bool) {
	trafficTrendCache.Lock()
	defer trafficTrendCache.Unlock()
	cached, ok := trafficTrendCache.items[key]
	if !ok || cached.buckets == nil || !at.Before(cached.expiresAt) {
		return nil, false
	}
	return cached.buckets, true
}

func storeTrafficTrend(key string, buckets []TrafficTrendBucket, at time.Time) {
	trafficTrendCache.Lock()
	defer trafficTrendCache.Unlock()
	if trafficTrendCache.items == nil {
		trafficTrendCache.items = make(map[string]trafficTrendCacheEntry)
	}
	if len(trafficTrendCache.items) >= trafficTrendCacheLimit {
		for cachedKey, cached := range trafficTrendCache.items {
			if at.After(cached.expiresAt) {
				delete(trafficTrendCache.items, cachedKey)
			}
		}
		if len(trafficTrendCache.items) >= trafficTrendCacheLimit {
			trafficTrendCache.items = make(map[string]trafficTrendCacheEntry)
		}
	}
	trafficTrendCache.items[key] = trafficTrendCacheEntry{expiresAt: at.Add(trafficTrendCacheTTL), buckets: buckets}
}

package metricstore

import "github.com/komari-monitor/komari/pkg/metric"

// SwapStoreForTest isolates component fixtures without opening production data.
// Callers must serialize tests using the process-wide metric store and restore it.
func SwapStoreForTest(next *metric.Store) func() {
	storeMu.Lock()
	previous := store
	store = next
	storeMu.Unlock()
	return func() { storeMu.Lock(); store = previous; storeMu.Unlock() }
}

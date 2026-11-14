package metricstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

func TestApplicationConfigurationRetiresLegacyStorage(t *testing.T) {
	for _, driver := range []string{"sqlite", "mysql", "postgresql"} {
		cfg, err := buildMetricConfig(&MetricStoreConfig{Driver: driver, DSN: "test-storage", DownsamplingEnabled: true}, true)
		if err != nil || !cfg.RetireLegacyStorage {
			t.Fatalf("%s: config=%+v error=%v", driver, cfg, err)
		}
	}
	ctx := context.Background()
	s, err := openStore(ctx, &MetricStoreConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "metrics.db"), DownsamplingEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Now().UTC().Add(-time.Hour)
	if err := s.Write(ctx, metric.Point{MetricName: MetricCPU, EntityID: "node", Timestamp: base, Value: 12}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Compact(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("legacy compaction: %d %v", n, err)
	}
	got, err := s.Query(ctx, metric.Query{MetricName: MetricCPU, EntityID: "node", Start: base.Add(-time.Second), End: base.Add(time.Second)})
	if err != nil || len(got) != 1 || got[0].Value != 12 {
		t.Fatalf("application history: %+v %v", got, err)
	}
}

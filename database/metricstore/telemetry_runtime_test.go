package metricstore

import (
	"testing"
	"time"
)

func TestTelemetryHotSafetyRetentionSettings(t *testing.T) {
	cfg, err := telemetryConfig(&MetricStoreConfig{})
	if err != nil || cfg.RawWindow != 15*time.Minute || cfg.RawRetention != 24*time.Hour {
		t.Fatal("runtime must separate online precision from safety retention", cfg, err)
	}
	for _, tc := range []struct {
		name              string
		online, retention int
		valid             bool
	}{
		{"short isolated fixture", 1, 1, true},
		{"normal one day", 15, 1440, true},
		{"maximum", 120, 10080, true},
		{"shorter than online", 120, 119, false},
		{"beyond limit", 15, 10081, false},
		{"negative", 15, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := telemetryConfig(&MetricStoreConfig{HotWindowMinutes: tc.online, HotRetentionMinutes: tc.retention})
			if (err == nil) != tc.valid {
				t.Fatal(cfg, err)
			}
			if err == nil && (cfg.RawWindow != time.Duration(tc.online)*time.Minute || cfg.RawRetention != time.Duration(tc.retention)*time.Minute) {
				t.Fatal("setting lost", cfg)
			}
		})
	}
}

package records

import (
	"testing"
	"time"
)

func TestParseDownsamplingDuration(t *testing.T) {
	tests := map[string]time.Duration{
		"1min": time.Minute,
		"2h":   2 * time.Hour,
		"3d":   72 * time.Hour,
		"4m":   4 * 30 * 24 * time.Hour,
		"5y":   5 * 365 * 24 * time.Hour,
	}
	for input, expected := range tests {
		actual, err := parseDownsamplingDuration(input)
		if err != nil {
			t.Fatalf("parseDownsamplingDuration(%q): %v", input, err)
		}
		if actual != expected {
			t.Fatalf("parseDownsamplingDuration(%q) = %s, want %s", input, actual, expected)
		}
	}

	for _, input := range []string{"", "0h", "1s", "1.5h", "h", "101y"} {
		if _, err := parseDownsamplingDuration(input); err == nil {
			t.Fatalf("parseDownsamplingDuration(%q) unexpectedly succeeded", input)
		}
	}
}

func TestParseDownsamplingPolicyValidatesTierOrder(t *testing.T) {
	policy := DefaultDownsamplingPolicy()
	if _, err := parseDownsamplingPolicy(policy); err != nil {
		t.Fatalf("default policy is invalid: %v", err)
	}

	policy.Tiers[1].Interval = "7min"
	if _, err := parseDownsamplingPolicy(policy); err == nil {
		t.Fatal("non-divisible tier interval unexpectedly accepted")
	}

	policy = DefaultDownsamplingPolicy()
	policy.Tiers[2].Retention = "2d"
	if _, err := parseDownsamplingPolicy(policy); err == nil {
		t.Fatal("decreasing retention unexpectedly accepted")
	}

	policy = DefaultDownsamplingPolicy()
	policy.RawRetention = "30min"
	policy.Tiers[0].Interval = "1h"
	policy.Tiers[1].Interval = "2h"
	policy.Tiers[2].Interval = "4h"
	parsed, err := parseDownsamplingPolicy(policy)
	if err != nil {
		t.Fatalf("raw retention shorter than the first interval was rejected: %v", err)
	}
	if parsed.rawRetention != 30*time.Minute {
		t.Fatalf("raw retention = %s, want 30m", parsed.rawRetention)
	}
}

package records

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/komari-monitor/komari/pkg/config"
)

const DownsamplingPolicyKey = "metric_downsampling_policy"

const maxDownsamplingDuration = 100 * 365 * 24 * time.Hour

type DownsamplingTier struct {
	Interval  string `json:"interval"`
	Retention string `json:"retention"`
}

type DownsamplingPolicy struct {
	Enabled      bool               `json:"enabled"`
	RawRetention string             `json:"raw_retention"`
	Tiers        []DownsamplingTier `json:"tiers"`
}

type parsedDownsamplingPolicy struct {
	rawRetention time.Duration
	tiers        []parsedDownsamplingTier
}

type parsedDownsamplingTier struct {
	interval  time.Duration
	retention time.Duration
}

func DefaultDownsamplingPolicy() DownsamplingPolicy {
	return DownsamplingPolicy{
		Enabled:      false,
		RawRetention: "1h",
		Tiers: []DownsamplingTier{
			{Interval: "1min", Retention: "24h"},
			{Interval: "5min", Retention: "7d"},
			{Interval: "1h", Retention: "90d"},
			{Interval: "1d", Retention: "5y"},
		},
	}
}

func GetDownsamplingPolicy() (DownsamplingPolicy, error) {
	policy, err := config.GetAs[DownsamplingPolicy](DownsamplingPolicyKey)
	if err != nil {
		policy = DefaultDownsamplingPolicy()
		if setErr := config.Set(DownsamplingPolicyKey, policy); setErr != nil {
			return DownsamplingPolicy{}, setErr
		}
	}
	if _, err := parseDownsamplingPolicy(policy); err != nil {
		return DownsamplingPolicy{}, err
	}
	return policy, nil
}

func SetDownsamplingPolicy(policy DownsamplingPolicy) error {
	if _, err := parseDownsamplingPolicy(policy); err != nil {
		return err
	}
	return config.Set(DownsamplingPolicyKey, policy)
}

func parseDownsamplingPolicy(policy DownsamplingPolicy) (parsedDownsamplingPolicy, error) {
	if len(policy.Tiers) != 4 {
		return parsedDownsamplingPolicy{}, errors.New("downsampling requires exactly four tiers")
	}
	rawRetention, err := parseDownsamplingDuration(policy.RawRetention)
	if err != nil {
		return parsedDownsamplingPolicy{}, fmt.Errorf("invalid raw retention: %w", err)
	}

	parsed := parsedDownsamplingPolicy{
		rawRetention: rawRetention,
		tiers:        make([]parsedDownsamplingTier, 0, len(policy.Tiers)),
	}
	var previousInterval, previousRetention time.Duration
	for index, tier := range policy.Tiers {
		interval, intervalErr := parseDownsamplingDuration(tier.Interval)
		if intervalErr != nil {
			return parsedDownsamplingPolicy{}, fmt.Errorf("invalid tier %d interval: %w", index+1, intervalErr)
		}
		retention, retentionErr := parseDownsamplingDuration(tier.Retention)
		if retentionErr != nil {
			return parsedDownsamplingPolicy{}, fmt.Errorf("invalid tier %d retention: %w", index+1, retentionErr)
		}
		if retention < interval {
			return parsedDownsamplingPolicy{}, fmt.Errorf("tier %d retention must not be shorter than its interval", index+1)
		}
		if index > 0 {
			if interval <= previousInterval || interval%previousInterval != 0 {
				return parsedDownsamplingPolicy{}, fmt.Errorf("tier %d interval must be a larger multiple of tier %d", index+1, index)
			}
			if retention < previousRetention {
				return parsedDownsamplingPolicy{}, fmt.Errorf("tier %d retention must not be shorter than tier %d", index+1, index)
			}
		}
		parsed.tiers = append(parsed.tiers, parsedDownsamplingTier{interval: interval, retention: retention})
		previousInterval = interval
		previousRetention = retention
	}
	return parsed, nil
}

func parseDownsamplingDuration(value string) (time.Duration, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	units := []struct {
		suffix string
		unit   time.Duration
	}{
		{"min", time.Minute},
		{"h", time.Hour},
		{"d", 24 * time.Hour},
		{"m", 30 * 24 * time.Hour},
		{"y", 365 * 24 * time.Hour},
	}
	for _, candidate := range units {
		if !strings.HasSuffix(value, candidate.suffix) {
			continue
		}
		number := strings.TrimSuffix(value, candidate.suffix)
		amount, err := strconv.ParseInt(number, 10, 64)
		if err != nil || amount <= 0 {
			return 0, errors.New("use a positive integer followed by min, h, d, m, or y")
		}
		if amount > int64(maxDownsamplingDuration/candidate.unit) {
			return 0, errors.New("duration exceeds 100 years")
		}
		return time.Duration(amount) * candidate.unit, nil
	}
	return 0, errors.New("use a positive integer followed by min, h, d, m, or y")
}

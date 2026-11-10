package jsonrpc

import (
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/assert"
)

func TestApplyThemeTrafficCompatibilityMapsLegacyFields(t *testing.T) {
	nodes := []models.Client{{
		TrafficLimit:          10,
		TrafficLimitType:      "max",
		EffectiveTrafficLimit: 12,
		EffectiveTrafficType:  "down",
	}}

	applyThemeTrafficCompatibility(nodes)

	assert.Equal(t, int64(12), nodes[0].TrafficLimit)
	assert.Equal(t, "down", nodes[0].TrafficLimitType)
	assert.Equal(t, int64(12), nodes[0].EffectiveTrafficLimit)
	assert.Equal(t, "down", nodes[0].EffectiveTrafficType)
}

func TestApplyThemeTrafficCompatibilityKeepsLegacyFallback(t *testing.T) {
	nodes := []models.Client{{TrafficLimit: 10, TrafficLimitType: "max"}}

	applyThemeTrafficCompatibility(nodes)

	assert.Equal(t, int64(10), nodes[0].TrafficLimit)
	assert.Equal(t, "max", nodes[0].TrafficLimitType)
}

func TestApplyThemeTrafficCompatibilityComputesNextReset(t *testing.T) {
	day := 15
	nodes := []models.Client{{
		TrafficLimit:         100,
		TrafficLimitType:     "sum",
		TrafficResetDay:      &day,
		TrafficResetTime:     "04:00:00",
		TrafficResetTimezone: "UTC",
	}}

	applyThemeTrafficCompatibility(nodes)

	assert.NotNil(t, nodes[0].TrafficNextReset)
	assert.True(t, nodes[0].TrafficNextReset.After(time.Now().Add(-24*time.Hour)))
}

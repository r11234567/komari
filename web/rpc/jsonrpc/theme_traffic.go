package jsonrpc

import (
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/trafficledger"
)

// applyThemeTrafficCompatibility keeps existing themes on the current billing
// cycle quota while the admin API continues to expose the configured base quota.
// It also provides the computed next reset instant for theme dashboards.
func applyThemeTrafficCompatibility(nodes []models.Client) {
	now := time.Now().UTC()
	for index := range nodes {
		node := &nodes[index]
		limit := node.EffectiveTrafficLimit
		if limit == 0 && node.TrafficLimit > 0 {
			limit = node.TrafficLimit
		}
		typeName := node.EffectiveTrafficType
		if typeName == "" {
			typeName = node.TrafficLimitType
		}
		node.TrafficLimit = limit
		node.TrafficLimitType = typeName

		schedule := trafficledger.DisplayTrafficSchedule(*node)
		if schedule.Active() {
			next := schedule.Next(now)
			node.TrafficNextReset = &next
		}
	}
}

package rpc

import (
	"slices"
	"time"
)

const ScopeNodesRead = "nodes:read"
const ScopeMetricsRead = "metrics:read"

type APITokenPolicy struct {
	ID        string
	OwnerUUID string
	Scopes    []string
	AgentIDs  []string
	AllAgents bool
	ExpiresAt time.Time
}

// Only individually reviewed read procedures are available. An admin role is
// never inferred from a token; future procedures remain denied by default.
func (p *Principal) AllowsAPIProcedure(procedure string) bool {
	if p == nil || p.APIToken == nil {
		return true
	}
	if !time.Now().Before(p.APIToken.ExpiresAt) {
		return false
	}
	var scope string
	switch procedure {
	case "/komari.browser.v1.BrowserService/ListAgents", "/komari.browser.v1.BrowserService/GetAgent":
		scope = ScopeNodesRead
	case "/komari.metrics.v1.MetricsService/QueryMetrics", "/komari.metrics.v1.MetricsService/GetPingStats", "/komari.metrics.v1.MetricsService/ListMetricDefinitions":
		scope = ScopeMetricsRead
	default:
		return false
	}
	return slices.Contains(p.APIToken.Scopes, scope)
}

func (p *Principal) AllowsAgent(agentID string) bool {
	return p == nil || p.APIToken == nil || time.Now().Before(p.APIToken.ExpiresAt) &&
		(p.APIToken.AllAgents || slices.Contains(p.APIToken.AgentIDs, agentID))
}

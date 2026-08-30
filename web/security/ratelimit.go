package security

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/config"
)

const (
	rateLimitMaxEntries = 4096
	rateLimitCleanupAge = 10 * time.Minute
)

type rateLimitBucket struct {
	tokens float64
	seen   time.Time
}

// RateLimitController applies conservative per-principal request budgets. It
// is deliberately disabled by default and can be changed without rebuilding
// the router through the site settings page.
type RateLimitController struct {
	mu      sync.Mutex
	enabled bool
	buckets map[string]rateLimitBucket
}

func NewRateLimitController(enabled bool) *RateLimitController {
	return &RateLimitController{enabled: enabled, buckets: make(map[string]rateLimitBucket)}
}

func (ctrl *RateLimitController) Update(event config.ConfigEvent) bool {
	changed, enabled := config.IsChangedT[bool](event, config.RateLimitEnabledKey)
	if !changed {
		return false
	}
	ctrl.mu.Lock()
	ctrl.enabled = enabled
	if !enabled {
		ctrl.buckets = make(map[string]rateLimitBucket)
	}
	ctrl.mu.Unlock()
	return true
}

func (ctrl *RateLimitController) allow(key string, rate, burst float64, now time.Time) bool {
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if !ctrl.enabled {
		return true
	}
	if len(ctrl.buckets) >= rateLimitMaxEntries {
		for bucketKey, bucket := range ctrl.buckets {
			if now.Sub(bucket.seen) > rateLimitCleanupAge {
				delete(ctrl.buckets, bucketKey)
			}
		}
	}
	bucket := ctrl.buckets[key]
	if bucket.seen.IsZero() {
		bucket.tokens = burst
	} else {
		bucket.tokens += now.Sub(bucket.seen).Seconds() * rate
		if bucket.tokens > burst {
			bucket.tokens = burst
		}
	}
	bucket.seen = now
	if bucket.tokens < 1 {
		ctrl.buckets[key] = bucket
		return false
	}
	bucket.tokens--
	ctrl.buckets[key] = bucket
	return true
}

func (ctrl *RateLimitController) snapshotEnabled() bool {
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	return ctrl.enabled
}

func (ctrl *RateLimitController) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !ctrl.snapshotEnabled() || !isAPIRequestPath(c.Request.URL.Path) ||
			c.Request.Method == http.MethodOptions || isLongLivedRequest(c.Request.URL.Path) {
			c.Next()
			return
		}
		key, rate, burst := ctrl.keyAndBudget(c)
		if ctrl.allow(key, rate, burst, time.Now()) {
			c.Next()
			return
		}
		c.Header("Retry-After", "1")
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "request rate limit exceeded"})
	}
}

func (ctrl *RateLimitController) keyAndBudget(c *gin.Context) (string, float64, float64) {
	role := c.GetString("role")
	identity := c.ClientIP()
	if client := c.GetString("client_uuid"); client != "" {
		identity = "client:" + client
	} else if uuid := c.GetString("uuid"); uuid != "" && uuid != "00000000-0000-0000-0000-000000000000" {
		identity = "user:" + uuid
	}
	path := c.Request.URL.Path
	if strings.HasPrefix(path, "/api/login") {
		return "login:" + identity, 0.2, 5
	}
	if role == "client" || strings.Contains(path, "/report") || strings.Contains(path, "/upload") {
		return "ingest:" + identity, 20, 40
	}
	// Count expensive history/dashboard families independently from ordinary
	// navigation. A complete dashboard is intentionally allowed to burst; only
	// sustained polling from many pages drains this budget and receives 429.
	if isHistoricalReadPath(path) {
		return "history:" + identity, 20, 120
	}
	return "read:" + identity, 40, 160
}

func isHistoricalReadPath(path string) bool {
	for _, suffix := range []string{
		"/QueryMetrics", "/GetPingStats", "/GetDashboardCharts",
		"/GetDashboardSummary", "/GetTrafficTrend",
	} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func isLongLivedRequest(path string) bool {
	for _, suffix := range []string{"/WatchAgentStatus", "/WatchDesiredConfig", "/WatchMetrics", "/StreamMetrics", "/WatchExecution", "/WatchSession", "/SubscribeEvents", "/WatchRescueSession", "/AttachSession", "/OpenSession"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

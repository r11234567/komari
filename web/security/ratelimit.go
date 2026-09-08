package security

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/config"
)

const (
	// rateLimitMaxEntries bounds the bucket table. Reaching it evicts, rather
	// than merely attempting a sweep: an earlier revision only deleted entries
	// idle for rateLimitCleanupAge, so a flood of distinct keys - which a
	// spoofable client identity makes trivial to produce - grew the table
	// without bound and turned every subsequent request into a full O(n) scan
	// under the shared mutex.
	rateLimitMaxEntries = 4096
	// rateLimitCleanupAge is how long an idle bucket is kept. A bucket at full
	// tokens carries no state worth preserving, so dropping it is equivalent to
	// never having seen the key.
	rateLimitCleanupAge = 10 * time.Minute
	// rateLimitSweepInterval throttles opportunistic sweeps so a table that
	// sits near capacity does not pay a scan on every single request.
	rateLimitSweepInterval = 30 * time.Second
)

type rateLimitBucket struct {
	tokens float64
	seen   time.Time
}

// RateLimitController applies conservative per-principal request budgets. It
// is deliberately disabled by default and can be changed without rebuilding
// the router through the site settings page.
type RateLimitController struct {
	mu        sync.Mutex
	enabled   bool
	buckets   map[string]rateLimitBucket
	lastSweep time.Time
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

// allow consumes one token from key's bucket and reports whether the request
// may proceed. When it returns false, the second value is how long the caller
// should wait before the next token is available.
func (ctrl *RateLimitController) allow(key string, rate, burst float64, now time.Time) (bool, time.Duration) {
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if !ctrl.enabled {
		return true, 0
	}
	ctrl.evictLocked(key, now)
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
		return false, retryAfterFor(bucket.tokens, rate)
	}
	bucket.tokens--
	ctrl.buckets[key] = bucket
	return true, 0
}

// retryAfterFor reports how long until the bucket holds a whole token. Serving
// a blanket one second would tell a caller throttled by the login budget - one
// token every five seconds - to come back four times too early, so a
// well-behaved client would generate four extra rejections per real attempt and
// inflate the 4xx rate that any log-driven banning layer counts.
func retryAfterFor(tokens, rate float64) time.Duration {
	if rate <= 0 {
		return time.Second
	}
	missing := 1 - tokens
	if missing <= 0 {
		return time.Second
	}
	seconds := math.Ceil(missing / rate)
	if seconds < 1 {
		seconds = 1
	}
	return time.Duration(seconds) * time.Second
}

// evictLocked keeps the bucket table bounded. It first sweeps idle entries, at
// most every rateLimitSweepInterval, and if the table is still full it drops an
// arbitrary entry so admission never depends on the table having room.
//
// Dropping a bucket only ever forgives a caller; it cannot manufacture a
// rejection for anyone. incoming is spared so a request cannot evict the very
// bucket it is about to charge.
func (ctrl *RateLimitController) evictLocked(incoming string, now time.Time) {
	if len(ctrl.buckets) < rateLimitMaxEntries {
		return
	}
	if now.Sub(ctrl.lastSweep) >= rateLimitSweepInterval {
		ctrl.lastSweep = now
		for key, bucket := range ctrl.buckets {
			if now.Sub(bucket.seen) > rateLimitCleanupAge {
				delete(ctrl.buckets, key)
			}
		}
	}
	// Every bucket is fresh: shed regardless, so the table cannot grow past its
	// bound and drag an O(n) scan onto each request under the shared mutex.
	for len(ctrl.buckets) >= rateLimitMaxEntries {
		evicted := false
		for key := range ctrl.buckets {
			if key == incoming {
				continue
			}
			delete(ctrl.buckets, key)
			evicted = true
			break
		}
		if !evicted {
			// The table holds only the incoming key. Nothing left to shed.
			return
		}
	}
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
		allowed, retryAfter := ctrl.allow(key, rate, burst, time.Now())
		if allowed {
			c.Next()
			return
		}
		c.Header("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
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

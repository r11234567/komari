package security

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRateLimitControllerCanBeToggled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controller := NewRateLimitController(false)
	router := gin.New()
	router.Use(controller.Middleware())
	router.GET("/api/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	for i := 0; i < 30; i++ {
		request := httptest.NewRequest(http.MethodGet, "/api/test", nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("disabled limiter rejected request %d with %d", i, response.Code)
		}
	}
}

func TestRateLimitControllerAllowsDashboardInitialBurst(t *testing.T) {
	controller := NewRateLimitController(true)
	now := time.Now()
	for i := 0; i < 30; i++ {
		if allowed, _ := controller.allow("history:user", 20, 120, now); !allowed {
			t.Fatalf("normal dashboard burst was limited at request %d", i+1)
		}
	}
}

func TestRateLimitControllerRejectsSustainedHistoryStorm(t *testing.T) {
	controller := NewRateLimitController(true)
	now := time.Now()
	for i := 0; i < 120; i++ {
		if allowed, _ := controller.allow("history:user", 20, 120, now); !allowed {
			t.Fatalf("history burst was limited too early at request %d", i+1)
		}
	}
	if allowed, _ := controller.allow("history:user", 20, 120, now); allowed {
		t.Fatal("sustained history storm was not limited")
	}
}

func TestRateLimitBucketTableStaysBounded(t *testing.T) {
	controller := NewRateLimitController(true)
	now := time.Now()
	// Distinct keys arriving faster than the idle age is the shape a spoofable
	// client address produces. An earlier revision only deleted entries idle
	// for rateLimitCleanupAge, so none of these were evictable and the table
	// grew without bound while every request paid an O(n) scan.
	for i := 0; i < rateLimitMaxEntries*3; i++ {
		controller.allow(fmt.Sprintf("read:%d", i), 40, 160, now)
	}
	controller.mu.Lock()
	size := len(controller.buckets)
	controller.mu.Unlock()
	if size > rateLimitMaxEntries {
		t.Fatalf("bucket table grew to %d entries, above the %d bound", size, rateLimitMaxEntries)
	}
}

func TestRateLimitEvictionKeepsIncomingKeyAccounted(t *testing.T) {
	controller := NewRateLimitController(true)
	now := time.Now()
	for i := 0; i < rateLimitMaxEntries; i++ {
		controller.allow(fmt.Sprintf("read:filler-%d", i), 40, 160, now)
	}
	// Eviction must never drop the bucket the current request is charging, or a
	// caller could stay unlimited by keeping the table full.
	const key = "login:victim"
	for i := 0; i < 5; i++ {
		if allowed, _ := controller.allow(key, 0.2, 5, now); !allowed {
			t.Fatalf("login burst was limited at request %d", i+1)
		}
	}
	if allowed, _ := controller.allow(key, 0.2, 5, now); allowed {
		t.Fatal("login budget was not enforced while the table was full")
	}
}

func TestRateLimitIdleBucketsAreSwept(t *testing.T) {
	controller := NewRateLimitController(true)
	start := time.Now()
	for i := 0; i < rateLimitMaxEntries; i++ {
		controller.allow(fmt.Sprintf("read:%d", i), 40, 160, start)
	}
	// Once the entries age past the idle window, a later request sweeps them
	// rather than merely shedding one at a time.
	controller.allow("read:new", 40, 160, start.Add(rateLimitCleanupAge+time.Minute))
	controller.mu.Lock()
	size := len(controller.buckets)
	controller.mu.Unlock()
	if size > 2 {
		t.Fatalf("idle sweep left %d entries", size)
	}
}

func TestRateLimitDisabledDoesNotAccumulateBuckets(t *testing.T) {
	controller := NewRateLimitController(false)
	now := time.Now()
	for i := 0; i < 100; i++ {
		if allowed, _ := controller.allow(fmt.Sprintf("read:%d", i), 40, 160, now); !allowed {
			t.Fatal("disabled limiter rejected a request")
		}
	}
	controller.mu.Lock()
	size := len(controller.buckets)
	controller.mu.Unlock()
	if size != 0 {
		t.Fatalf("disabled limiter retained %d buckets", size)
	}
}

func TestRateLimitBucketsRefillOverTime(t *testing.T) {
	controller := NewRateLimitController(true)
	now := time.Now()
	for i := 0; i < 5; i++ {
		controller.allow("login:user", 0.2, 5, now)
	}
	if allowed, _ := controller.allow("login:user", 0.2, 5, now); allowed {
		t.Fatal("login budget was not exhausted")
	}
	// 0.2 tokens per second means a whole token after five.
	if allowed, _ := controller.allow("login:user", 0.2, 5, now.Add(6*time.Second)); !allowed {
		t.Fatal("login bucket did not refill")
	}
}

func TestRetryAfterMatchesBucketRefillRate(t *testing.T) {
	controller := NewRateLimitController(true)
	now := time.Now()
	for i := 0; i < 5; i++ {
		controller.allow("login:user", 0.2, 5, now)
	}
	_, retryAfter := controller.allow("login:user", 0.2, 5, now)
	// A blanket one second would send a well-behaved client back four times
	// too early on the login budget, and each premature attempt is another 4xx
	// for a log-driven banning layer to count.
	if retryAfter < 4*time.Second {
		t.Fatalf("Retry-After of %v is shorter than the 5s the login bucket needs", retryAfter)
	}
	if retryAfter > 10*time.Second {
		t.Fatalf("Retry-After of %v overstates the login bucket refill", retryAfter)
	}
}

func TestRetryAfterForHandlesDegenerateRate(t *testing.T) {
	// A zero or negative rate would divide by zero; the helper must still
	// return a usable positive delay.
	if got := retryAfterFor(0, 0); got < time.Second {
		t.Fatalf("retryAfterFor with a zero rate returned %v", got)
	}
	if got := retryAfterFor(0.5, -1); got < time.Second {
		t.Fatalf("retryAfterFor with a negative rate returned %v", got)
	}
}

func TestRateLimitMiddlewareServesHonestRetryAfterHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controller := NewRateLimitController(true)
	router := gin.New()
	router.Use(controller.Middleware())
	// With no identity middleware the login budget keys on the client address,
	// which is the case that matters: an unauthenticated login flood.
	router.POST("/api/login", func(c *gin.Context) { c.Status(http.StatusOK) })

	var lastCode int
	var retryAfter string
	for i := 0; i < 8; i++ {
		request := httptest.NewRequest(http.MethodPost, "/api/login", nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		lastCode = response.Code
		retryAfter = response.Header().Get("Retry-After")
	}
	if lastCode != http.StatusTooManyRequests {
		t.Fatalf("login flood ended with status %d, want 429", lastCode)
	}
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil {
		t.Fatalf("Retry-After %q is not an integer count of seconds: %v", retryAfter, err)
	}
	if seconds < 2 {
		t.Fatalf("Retry-After of %ds understates the login bucket refill", seconds)
	}
}

func TestRateLimitSkipsLongLivedAndPreflightRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controller := NewRateLimitController(true)
	router := gin.New()
	router.Use(controller.Middleware())
	handler := func(c *gin.Context) { c.Status(http.StatusNoContent) }
	router.GET("/komari.agent.v1.AgentEventService/SubscribeEvents", handler)
	router.OPTIONS("/api/anything", handler)

	// A durable subscription occupies one connection for its lifetime; charging
	// it per re-subscribe would throttle exactly the traffic that must persist.
	for i := 0; i < 300; i++ {
		request := httptest.NewRequest(http.MethodGet, "/komari.agent.v1.AgentEventService/SubscribeEvents", nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("long-lived request %d was limited with %d", i, response.Code)
		}
	}
	for i := 0; i < 300; i++ {
		request := httptest.NewRequest(http.MethodOptions, "/api/anything", nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("preflight %d was limited with %d", i, response.Code)
		}
	}
}

func TestRateLimitIgnoresNonAPIPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controller := NewRateLimitController(true)
	router := gin.New()
	router.Use(controller.Middleware())
	router.GET("/assets/app.js", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	for i := 0; i < 300; i++ {
		request := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("static asset request %d was limited with %d", i, response.Code)
		}
	}
}

func TestAgentIngestBudgetToleratesDefaultReportInterval(t *testing.T) {
	controller := NewRateLimitController(true)
	now := time.Now()
	// The Agent's default report cadence is one submission every three seconds.
	// A whole hour of it must not come near the ingest budget, or enabling the
	// switch would knock healthy Agents offline.
	for i := 0; i < 1200; i++ {
		at := now.Add(time.Duration(i) * 3 * time.Second)
		if allowed, _ := controller.allow("ingest:client:node-a", 20, 40, at); !allowed {
			t.Fatalf("healthy agent reporting was limited at submission %d", i+1)
		}
	}
}

func TestRateLimitKeysSeparatePrincipals(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controller := NewRateLimitController(true)

	newContext := func(path string, setup func(*gin.Context)) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, path, nil)
		if setup != nil {
			setup(c)
		}
		return c
	}

	agentKey, agentRate, _ := controller.keyAndBudget(newContext("/komari.report.v1.AgentReportService/SubmitReport", func(c *gin.Context) {
		c.Set("role", "client")
		c.Set("client_uuid", "node-a")
	}))
	if agentKey != "ingest:client:node-a" {
		t.Fatalf("agent key = %q", agentKey)
	}
	if agentRate != 20 {
		t.Fatalf("agent rate = %v", agentRate)
	}

	userKey, _, _ := controller.keyAndBudget(newContext("/komari.metrics.v1.MetricsService/QueryMetrics", func(c *gin.Context) {
		c.Set("uuid", "user-1")
	}))
	if userKey != "history:user:user-1" {
		t.Fatalf("user history key = %q", userKey)
	}

	// The placeholder UUID stands for an API key call and must not collapse
	// every such caller into one shared bucket keyed on a fake identity.
	apiKeyContext := newContext("/api/admin/client/list", func(c *gin.Context) {
		c.Set("uuid", "00000000-0000-0000-0000-000000000000")
	})
	apiKey, _, _ := controller.keyAndBudget(apiKeyContext)
	if apiKey == "read:user:00000000-0000-0000-0000-000000000000" {
		t.Fatal("placeholder UUID was used as a bucket identity")
	}

	loginKey, loginRate, loginBurst := controller.keyAndBudget(newContext("/api/login", nil))
	if loginRate != 0.2 || loginBurst != 5 {
		t.Fatalf("login budget = %v/%v", loginRate, loginBurst)
	}
	if got := "login:"; len(loginKey) < len(got) || loginKey[:len(got)] != got {
		t.Fatalf("login key = %q", loginKey)
	}
}

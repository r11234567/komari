package security

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestAPIKeyMatchesRejectsPartialMalformedAndEmptyCredentials(t *testing.T) {
	key := "komari-synthetic-256-bit-secret"
	for _, header := range []string{"", key, "Bearer ", "bearer " + key, "Bearer " + key[:len(key)-1], "Bearer " + key + "x", "Bearer " + key + "\x00"} {
		if APIKeyMatches(header, key) {
			t.Fatalf("invalid header accepted: %q", header)
		}
	}
	if !APIKeyMatches("Bearer "+key, key) {
		t.Fatal("valid credential rejected")
	}
	if APIKeyMatches("Bearer short", "short") || APIKeyMatches("Bearer ", "") {
		t.Fatal("unconfigured/short legacy key accepted")
	}
}

func TestAPIFailureBudgetDoesNotChargeSuccessfulTrafficAndRecovers(t *testing.T) {
	limiter := NewAPIKeyAttemptLimiter()
	now := time.Now()
	for i := 0; i < 1000; i++ {
		if ok, _ := limiter.Allow("source", now); !ok {
			t.Fatal("successful API traffic charged as failure")
		}
	}
	for i := 0; i < 10; i++ {
		if ok, _ := limiter.Allow("source", now); !ok {
			t.Fatal("burst rejected too early")
		}
		limiter.Failure("source", now)
	}
	if ok, retry := limiter.Allow("source", now); ok || retry != 6*time.Second {
		t.Fatalf("budget not exhausted: %t %s", ok, retry)
	}
	if ok, _ := limiter.Allow("other", now); !ok {
		t.Fatal("unrelated source locked")
	}
	if ok, _ := limiter.Allow("source", now.Add(6*time.Second)); !ok {
		t.Fatal("budget did not replenish")
	}
	for i := 0; i < rateLimitMaxEntries*2; i++ {
		limiter.Failure(fmt.Sprint(i), now)
	}
	if len(limiter.budget.buckets) > rateLimitMaxEntries {
		t.Fatal("failure state is unbounded")
	}
}

func TestAPISourceIgnoresSpoofedForwardingWithoutExplicitProxyTrust(t *testing.T) {
	for _, trust := range []string{"", "none", LoopbackProxies} {
		t.Run(trust, func(t *testing.T) {
			t.Setenv(TrustedProxyEnv, trust)
			router := gin.New()
			if err := ConfigureTrustedProxies(router, trust); err != nil {
				t.Fatal(err)
			}
			router.GET("/", func(c *gin.Context) { c.String(200, AuthSourceIP(c)) })
			request := httptest.NewRequest("GET", "/", nil)
			request.RemoteAddr = "203.0.113.8:12345"
			request.Header.Set("X-Forwarded-For", "192.0.2.99")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Body.String() != "203.0.113.8" {
				t.Fatalf("caller chose auth source: %s", response.Body.String())
			}
		})
	}
}

func TestAPITokenUsageHasIndependentBoundedHistoryBudget(t *testing.T) {
	limiter := NewAPITokenUsageLimiter()
	now := time.Now()
	for i := 0; i < historyReadBurst; i++ {
		if ok, _ := limiter.Allow("one", "/komari.metrics.v1.MetricsService/QueryMetrics", now); !ok {
			t.Fatal("history burst rejected early")
		}
	}
	if ok, _ := limiter.Allow("one", "/komari.metrics.v1.MetricsService/GetPingStats", now); ok {
		t.Fatal("changing history endpoint bypassed token budget")
	}
	if ok, _ := limiter.Allow("two", "/komari.metrics.v1.MetricsService/QueryMetrics", now); !ok {
		t.Fatal("unrelated token blocked")
	}
	if ok, _ := limiter.Allow("one", "/komari.browser.v1.BrowserService/GetAgent", now); !ok {
		t.Fatal("history exhausted ordinary read lane")
	}
}

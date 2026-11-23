package security

import (
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

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
			if err := ConfigureRequestMetadata(router); err != nil {
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

func TestUnknownAgentProbeBudgetsAreChargedBeforeConcurrentLookups(t *testing.T) {
	for _, tc := range []struct {
		name      string
		uniqueIPs bool
		admitted  int32
	}{
		{name: "shared source", admitted: 20},
		{name: "multiple sources", uniqueIPs: true, admitted: 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limiter := NewAPIKeyAttemptLimiter()
			now := time.Now()
			var count atomic.Int32
			var wg sync.WaitGroup
			for i := range 100 {
				wg.Go(func() {
					source := "shared"
					if tc.uniqueIPs {
						source = fmt.Sprint(i)
					}
					if ok, _ := limiter.AllowUnknownAgentProbe(source, now); ok {
						count.Add(1)
					}
				})
			}
			wg.Wait()
			if got := count.Load(); got != tc.admitted {
				t.Fatalf("admitted %d concurrent probes, want %d", got, tc.admitted)
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

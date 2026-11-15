package security

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestHistoryInflightBoundReleasesAfterClientCancellation(t *testing.T) {
	controller := NewRateLimitController(true)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("uuid", "reader"); c.Next() }, controller.Middleware())
	entered := make(chan struct{}, historyReadPerIdentity)
	release := make(chan struct{})
	otherDone := make(chan struct{}, historyReadPerIdentity-1)
	defer func() {
		close(release)
		for range historyReadPerIdentity - 1 {
			select {
			case <-otherDone:
			case <-time.After(time.Second):
				t.Error("reader did not release its slot")
			}
		}
	}()
	const path = "/komari.metrics.v1.MetricsService/QueryMetrics"
	handler := func(c *gin.Context) {
		if c.Query("block") == "1" {
			entered <- struct{}{}
			select {
			case <-release:
			case <-c.Request.Context().Done():
			}
		}
		c.Status(http.StatusOK)
	}
	router.POST(path, handler)
	const ping = "/komari.metrics.v1.MetricsService/GetPingStats"
	router.POST(ping, handler)
	firstCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path+"?block=1", nil).WithContext(firstCtx))
		close(done)
	}()
	for i := 0; i < historyReadPerIdentity-1; i++ {
		go func(i int) {
			readPath := path
			if i%2 == 0 {
				readPath = ping
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, readPath+"?block=1", nil))
			if response.Code != http.StatusOK {
				t.Errorf("normal concurrent chart/Ping rejected: %d", response.Code)
			}
			otherDone <- struct{}{}
		}(i)
	}
	for range historyReadPerIdentity {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("normal chart/Ping fan-out did not enter the handlers")
		}
	}
	rejected := httptest.NewRecorder()
	router.ServeHTTP(rejected, httptest.NewRequest(http.MethodPost, path, nil))
	if rejected.Code != http.StatusTooManyRequests || rejected.Header().Get("Retry-After") == "" {
		t.Fatal("excess in-flight read was admitted")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled client retained its request slot")
	}
	accepted := httptest.NewRecorder()
	router.ServeHTTP(accepted, httptest.NewRequest(http.MethodPost, path, nil))
	if accepted.Code != http.StatusOK {
		t.Fatalf("cancelled slot was not reusable: %d", accepted.Code)
	}
}

func TestHistoryGlobalBoundPreservesAgentRequests(t *testing.T) {
	controller := NewRateLimitController(true)
	var held []string
	defer func() {
		for _, key := range held {
			controller.releaseHistory(key)
		}
	}()
	// Distinct principals must still share a finite server-wide bound.
	for i := 0; i < 512; i++ {
		key := fmt.Sprintf("history:user:%d", i)
		if !controller.acquireHistory(key) {
			break
		}
		held = append(held, key)
	}
	if len(held) < 64 || len(held) > 256 {
		t.Fatalf("unexpected global capacity: %d", len(held))
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("role", "client")
		c.Set("client_uuid", "reporting-agent")
		c.Next()
	}, controller.Middleware())
	for _, path := range []string{
		"/komari.metrics.v1.MetricsService/SubmitMetrics",
		"/komari.network.v1.NetworkProbeService/SubmitPingProbeResult",
	} {
		router.POST(path, func(c *gin.Context) { c.Status(http.StatusOK) })
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("full history capacity blocked Agent request %s: %d", path, response.Code)
		}
	}
}

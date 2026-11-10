package security

import (
	"context"
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
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	secondDone := make(chan struct{})
	defer func() {
		close(release)
		select {
		case <-secondDone:
		case <-time.After(time.Second):
			t.Error("second reader did not release its slot")
		}
	}()
	const path = "/komari.metrics.v1.MetricsService/QueryMetrics"
	router.POST(path, func(c *gin.Context) {
		if c.Query("block") == "1" {
			entered <- struct{}{}
			select {
			case <-release:
			case <-c.Request.Context().Done():
			}
		}
		c.Status(http.StatusOK)
	})
	firstCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path+"?block=1", nil).WithContext(firstCtx))
		close(done)
	}()
	go func() {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path+"?block=1", nil))
		close(secondDone)
	}()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("fixture did not enter two permitted reads")
		}
	}
	rejected := httptest.NewRecorder()
	router.ServeHTTP(rejected, httptest.NewRequest(http.MethodPost, path, nil))
	if rejected.Code != http.StatusTooManyRequests || rejected.Header().Get("Retry-After") == "" {
		t.Fatal("a third in-flight read was admitted")
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

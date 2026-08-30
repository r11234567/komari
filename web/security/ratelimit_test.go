package security

import (
	"net/http"
	"net/http/httptest"
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
		if !controller.allow("history:user", 20, 120, now) {
			t.Fatalf("normal dashboard burst was limited at request %d", i+1)
		}
	}
}

func TestRateLimitControllerRejectsSustainedHistoryStorm(t *testing.T) {
	controller := NewRateLimitController(true)
	now := time.Now()
	for i := 0; i < 120; i++ {
		if !controller.allow("history:user", 20, 120, now) {
			t.Fatalf("history burst was limited too early at request %d", i+1)
		}
	}
	if controller.allow("history:user", 20, 120, now) {
		t.Fatal("sustained history storm was not limited")
	}
}

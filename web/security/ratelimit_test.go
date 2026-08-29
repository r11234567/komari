package security

import (
	"net/http"
	"net/http/httptest"
	"testing"

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

func TestRateLimitControllerRejectsBurstWhenEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controller := NewRateLimitController(true)
	router := gin.New()
	router.Use(controller.Middleware())
	router.GET("/api/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	for i := 0; i < 16; i++ {
		request := httptest.NewRequest(http.MethodGet, "/api/test", nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if i < 16 && response.Code != http.StatusNoContent && response.Code != http.StatusTooManyRequests {
			t.Fatalf("unexpected status %d", response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("burst was not limited, got %d", response.Code)
	}
}

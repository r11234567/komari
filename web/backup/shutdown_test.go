package backup

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestGracefulRestartHandlerAndMutationRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ResetRestartStateForTest()
	t.Cleanup(ResetRestartStateForTest)

	var gotReason string
	RegisterRestartHandler(func(reason string) {
		gotReason = reason
	})

	r := gin.New()
	r.Use(RejectMutationsWhileRestarting())
	r.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })
	r.POST("/api/mutate", func(c *gin.Context) { c.String(http.StatusOK, "mutated") })

	// Before restart is requested, mutations succeed.
	req := httptest.NewRequest(http.MethodPost, "/api/mutate", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 before restart, got %d", rec.Code)
	}

	RequestGracefulRestart("unit_test_backup")
	if gotReason != "unit_test_backup" {
		t.Fatalf("handler reason = %q, want unit_test_backup", gotReason)
	}
	if !IsRestartPending() {
		t.Fatal("expected IsRestartPending() to be true")
	}

	// Mutations are rejected with 503 + Retry-After while reads continue cleanly.
	reqMut := httptest.NewRequest(http.MethodPost, "/api/mutate", nil)
	recMut := httptest.NewRecorder()
	r.ServeHTTP(recMut, reqMut)
	if recMut.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 during restart drain, got %d", recMut.Code)
	}
	if recMut.Header().Get("Retry-After") != "5" {
		t.Fatalf("Retry-After = %q, want 5", recMut.Header().Get("Retry-After"))
	}

	reqRead := httptest.NewRequest(http.MethodGet, "/ping", nil)
	recRead := httptest.NewRecorder()
	r.ServeHTTP(recRead, reqRead)
	if recRead.Code != http.StatusOK {
		t.Fatalf("expected 200 for GET during restart drain, got %d", recRead.Code)
	}
}

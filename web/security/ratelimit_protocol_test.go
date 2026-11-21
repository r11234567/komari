package security

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRateLimitConnectCeremonyProtocolAndIndependentBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctrl := NewRateLimitController(false)
	router := gin.New()
	if err := ConfigureRequestMetadata(router); err != nil {
		t.Fatal(err)
	}
	router.Use(ctrl.Middleware())
	login := "/komari.credential.v1.PasswordLoginService/BeginPasswordLogin"
	router.POST(login, func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodPost, login, nil)
		req.RemoteAddr = "198.51.100.20:1234"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		last = rec
		if i < 5 && rec.Code != http.StatusOK {
			t.Fatalf("attempt %d unexpectedly limited: %d", i, rec.Code)
		}
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", last.Code)
	}
	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(last.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal Connect error: %v", err)
	}
	if payload.Code != "resource_exhausted" || payload.Message == "" {
		t.Fatalf("unexpected Connect error payload: %+v", payload)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header on throttled ceremony")
	}
}

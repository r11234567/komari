package security

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCorsMiddlewareValidatesAPIOrigins(t *testing.T) {
	router := setupCORSRouter(true, "https://allowed.example")

	tests := []struct {
		name            string
		url             string
		origin          string
		wantStatus      int
		wantAllowOrigin string
	}{
		{
			name:       "allows API requests without Origin",
			url:        "https://api.example/api/ping",
			wantStatus: http.StatusOK,
		},
		{
			name:            "allows same scheme and host Origin",
			url:             "https://api.example/api/ping",
			origin:          "https://api.example",
			wantStatus:      http.StatusOK,
			wantAllowOrigin: "https://api.example",
		},
		{
			name:       "rejects scheme downgrade Origin",
			url:        "http://api.example/api/ping",
			origin:     "https://api.example",
			wantStatus: http.StatusForbidden,
		},
		{
			name:            "allows configured Origin",
			url:             "https://api.example/api/ping",
			origin:          "https://allowed.example",
			wantStatus:      http.StatusOK,
			wantAllowOrigin: "https://allowed.example",
		},
		{
			name:       "rejects unlisted Origin",
			url:        "https://api.example/api/ping",
			origin:     "https://evil.example",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := performCORSRequest(router, http.MethodGet, tt.url, "api.example", tt.origin)

			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, tt.wantStatus)
			}
			if got := response.Header().Get("Access-Control-Allow-Origin"); got != tt.wantAllowOrigin {
				t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, tt.wantAllowOrigin)
			}
		})
	}
}

func TestCorsMiddlewareRejectsUnverifiedBearerFromForeignOrigin(t *testing.T) {
	router := setupCORSRouter(true, "")

	request := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	request.Host = "api.example"
	request.Header.Set("Origin", "https://evil.example")
	request.Header.Set("Authorization", "Bearer 123456789012")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden || response.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("retired key gained cross-origin access: %d", response.Code)
	}
}

func TestCorsMiddlewareHandlesAPIPreflight(t *testing.T) {
	router := setupCORSRouter(true, "https://allowed.example")

	allowedReq := httptest.NewRequest(http.MethodOptions, "https://api.example/api/ping", nil)
	allowedReq.Host = "api.example"
	allowedReq.Header.Set("Origin", "https://allowed.example")
	allowedReq.Header.Set("Access-Control-Request-Method", http.MethodPost)
	allowed := httptest.NewRecorder()
	router.ServeHTTP(allowed, allowedReq)
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("allowed preflight status = %d, want %d", allowed.Code, http.StatusNoContent)
	}
	if got := allowed.Header().Get("Access-Control-Allow-Origin"); got != "https://allowed.example" {
		t.Fatalf("allowed preflight Access-Control-Allow-Origin = %q", got)
	}

	rejected := performCORSRequest(router, http.MethodOptions, "https://api.example/api/ping", "api.example", "https://evil.example")
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("rejected preflight status = %d, want %d", rejected.Code, http.StatusForbidden)
	}
}

func TestCorsMiddlewareProtectsConnectProcedures(t *testing.T) {
	router := setupCORSRouter(true, "https://allowed.example")
	router.POST("/komari.browser.v1.BrowserService/GetPublicInfo", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	rejected := performCORSRequest(router, http.MethodPost, "/komari.browser.v1.BrowserService/GetPublicInfo", "api.example", "https://evil.example")
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("Connect origin status = %d, want %d", rejected.Code, http.StatusForbidden)
	}
}

func TestCorsMiddlewareRejectsForeignAuthorizationPreflight(t *testing.T) {
	router := setupCORSRouter(true, "")

	request := httptest.NewRequest(http.MethodOptions, "https://api.example/api/ping", nil)
	request.Host = "api.example"
	request.Header.Set("Origin", "https://evil.example")
	request.Header.Set("Access-Control-Request-Method", http.MethodGet)
	request.Header.Set("Access-Control-Request-Headers", "authorization")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
	}
	if got := response.Header().Get("Vary"); got == "" {
		t.Fatal("expected Vary headers even when rejecting preflight")
	}
}

func TestCorsMiddlewareAllowsAuthorizationPreflightFromAllowedOrigin(t *testing.T) {
	router := setupCORSRouter(true, "https://allowed.example")

	request := httptest.NewRequest(http.MethodOptions, "https://api.example/api/ping", nil)
	request.Host = "api.example"
	request.Header.Set("Origin", "https://allowed.example")
	request.Header.Set("Access-Control-Request-Method", http.MethodGet)
	request.Header.Set("Access-Control-Request-Headers", "authorization")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "https://allowed.example" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, "https://allowed.example")
	}
	if got := response.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Access-Control-Allow-Credentials = %q, want true", got)
	}
}

func TestCorsMiddlewareDisabledSkipsAPIValidation(t *testing.T) {
	router := setupCORSRouter(false, "")

	response := performCORSRequest(router, http.MethodGet, "/api/ping", "api.example", "https://evil.example")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

func TestCorsMiddlewareSkipsNonAPIPaths(t *testing.T) {
	router := setupCORSRouter(true, "")

	response := performCORSRequest(router, http.MethodGet, "/public", "api.example", "https://evil.example")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

func setupCORSRouter(enabled bool, allowlist string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(NewCorsController(enabled, allowlist).Middleware())
	router.GET("/api/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	router.GET("/public", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	return router
}

func performCORSRequest(handler http.Handler, method, path, host, origin string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.Host = host
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

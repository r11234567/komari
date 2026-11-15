package security

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/config"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestCorsMiddlewareValidatesAPIOrigins(t *testing.T) {
	setupCORSConfigDB(t, "")
	router := setupCORSRouter(true, "https://allowed.example")

	tests := []struct {
		name            string
		origin          string
		wantStatus      int
		wantAllowOrigin string
	}{
		{
			name:       "allows API requests without Origin",
			wantStatus: http.StatusOK,
		},
		{
			name:            "allows same host Origin",
			origin:          "https://api.example",
			wantStatus:      http.StatusOK,
			wantAllowOrigin: "https://api.example",
		},
		{
			name:            "allows configured Origin",
			origin:          "https://allowed.example",
			wantStatus:      http.StatusOK,
			wantAllowOrigin: "https://allowed.example",
		},
		{
			name:       "rejects unlisted Origin",
			origin:     "https://evil.example",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := performCORSRequest(router, http.MethodGet, "/api/ping", "api.example", tt.origin)

			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, tt.wantStatus)
			}
			if got := response.Header().Get("Access-Control-Allow-Origin"); got != tt.wantAllowOrigin {
				t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, tt.wantAllowOrigin)
			}
		})
	}
}

func TestCorsMiddlewareAPIKeyCannotAuthorizeOrigin(t *testing.T) {
	setupCORSConfigDB(t, "123456789012")
	router := setupCORSRouter(true, "")

	request := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	request.Host = "api.example"
	request.Header.Set("Origin", "https://evil.example")
	request.Header.Set("Authorization", "Bearer 123456789012")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("credential bypassed origin policy: %q", got)
	}
}

func TestCorsMiddlewareHandlesAPIPreflight(t *testing.T) {
	setupCORSConfigDB(t, "")
	router := setupCORSRouter(true, "https://allowed.example")

	allowed := performCORSRequest(router, http.MethodOptions, "/api/ping", "api.example", "https://allowed.example")
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("allowed preflight status = %d, want %d", allowed.Code, http.StatusNoContent)
	}
	if got := allowed.Header().Get("Access-Control-Allow-Origin"); got != "https://allowed.example" {
		t.Fatalf("allowed preflight Access-Control-Allow-Origin = %q", got)
	}

	rejected := performCORSRequest(router, http.MethodOptions, "/api/ping", "api.example", "https://evil.example")
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("rejected preflight status = %d, want %d", rejected.Code, http.StatusForbidden)
	}
}

func TestCorsMiddlewareProtectsConnectProcedures(t *testing.T) {
	setupCORSConfigDB(t, "")
	router := setupCORSRouter(true, "https://allowed.example")
	router.POST("/komari.browser.v1.BrowserService/GetPublicInfo", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	rejected := performCORSRequest(router, http.MethodPost, "/komari.browser.v1.BrowserService/GetPublicInfo", "api.example", "https://evil.example")
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("Connect origin status = %d, want %d", rejected.Code, http.StatusForbidden)
	}
}

func TestCorsMiddlewareAuthorizationHeaderCannotAuthorizeOrigin(t *testing.T) {
	setupCORSConfigDB(t, "")
	router := setupCORSRouter(true, "")

	request := httptest.NewRequest(http.MethodOptions, "/api/ping", nil)
	request.Host = "api.example"
	request.Header.Set("Origin", "https://evil.example")
	request.Header.Set("Access-Control-Request-Headers", "authorization")
	request.Header.Set("Access-Control-Request-Method", "POST")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("requested authorization header bypassed origin policy: %q", got)
	}
	if got := response.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("Access-Control-Allow-Credentials = %q, want empty", got)
	}
}

func TestCorsMiddlewareDisabledSkipsAPIValidation(t *testing.T) {
	setupCORSConfigDB(t, "")
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
	setupCORSConfigDB(t, "")
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

func setupCORSConfigDB(t *testing.T, apiKey string) {
	t.Helper()

	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite test db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sqlite test db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	config.SetDb(db)
	if apiKey != "" {
		if err := config.Set(config.ApiKeyKey, apiKey); err != nil {
			t.Fatalf("set api key: %v", err)
		}
	}
}

func performCORSRequest(handler http.Handler, method, path, host, origin string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "https://"+host+path, nil)
	request.Host = host
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if method == http.MethodOptions {
		request.Header.Set("Access-Control-Request-Method", "POST")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestCorsPreflightValidatesRPCHeadersAndPreservesVary(t *testing.T) {
	for _, tc := range []struct {
		method, headers string
		status          int
	}{
		{"POST", "authorization,content-type,connect-protocol-version,connect-timeout-ms,x-komari-workspace-grant,x-komari-credential-authorization,x-bind-ticket", 204},
		{"POST", "x-unregistered-credential", 403},
		{"POST", "Set-Cookie", 403},
		{"TRACE", "content-type", 403},
		{"", "content-type", 403},
	} {
		router := gin.New()
		router.Use(func(c *gin.Context) { c.Header("Vary", "Accept-Encoding"); c.Next() })
		router.Use(NewCorsController(true, "https://console.example").Middleware())
		router.POST("/komari.test.Service/Call", func(c *gin.Context) { t.Fatal("preflight reached handler") })
		r := httptest.NewRequest("OPTIONS", "https://api.example/komari.test.Service/Call", nil)
		r.Header.Set("Origin", "https://console.example")
		r.Header.Set("Access-Control-Request-Method", tc.method)
		r.Header.Set("Access-Control-Request-Headers", tc.headers)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: status %d", tc.method, tc.headers, w.Code)
		}
		vary := strings.Join(w.Header().Values("Vary"), ",")
		for _, field := range []string{"Accept-Encoding", "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
			if !strings.Contains(vary, field) {
				t.Fatalf("Vary lost %s: %s", field, vary)
			}
		}
		if strings.Contains(strings.ToLower(w.Header().Get("Access-Control-Expose-Headers")), "set-cookie") {
			t.Fatal("exposed forbidden cookie header")
		}
	}
}

func TestOriginAllowlistRequiresExactSchemeHostAndPort(t *testing.T) {
	for _, entry := range []string{"*", "console.example", "http://console.example", "https://console.example:8443", "https://console.example/path"} {
		if OriginInAllowlist("https://console.example", entry) {
			t.Fatalf("non-exact origin authorized: %q", entry)
		}
	}
	if !OriginInAllowlist("https://console.example", "https://console.example:443") {
		t.Fatal("default-port normalization lost valid origin")
	}
	if OriginInAllowlist("null", "*") {
		t.Fatal("opaque origin was authorized")
	}
}

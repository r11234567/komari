package public

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
)

func TestLegacyLoginTOTPFailureAndReplayNeverIssueCookie(t *testing.T) {
	user, err := accounts.CreateAccount("advisory-login", "Synthetic-password-123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = accounts.DeleteAccountByUsername(user.Username) })
	const secret = "JBSWY3DPEHPK3PXP"
	if err = accounts.Enable2Fa(user.UUID, secret); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/login", Login)
	login := func(code string, want int) {
		t.Helper()
		body, _ := json.Marshal(LoginRequest{Username: user.Username, Password: "Synthetic-password-123", TwoFa: code})
		req := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("login status=%d want=%d", w.Code, want)
		}
		if want != 200 && (len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), "session_token")) {
			t.Fatal("failed factor issued a session")
		}
	}
	login("", 401)
	login("not-a-code", 401)
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	login(code, 200)
	login(code, 401)
	if err = accounts.Enable2Fa(user.UUID, "!invalid-secret!"); err != nil {
		t.Fatal(err)
	}
	login("123456", 401)
}

func TestLogin(t *testing.T) {
	// 设置测试模式
	gin.SetMode(gin.TestMode)
	accounts.CreateAccount("testuser", "correctpassword")
	tests := []struct {
		name           string
		requestBody    LoginRequest
		expectedStatus int
		expectedBody   map[string]interface{}
	}{
		{
			name: "成功登录",
			requestBody: LoginRequest{
				Username: "testuser",
				Password: "correctpassword",
			},
			expectedStatus: http.StatusOK,
		},
		{
			name: "无效的请求体",
			requestBody: LoginRequest{
				Username: "",
				Password: "",
			},
			expectedStatus: http.StatusBadRequest,
			expectedBody: map[string]interface{}{
				"status":  "error",
				"message": "Invalid request body: Username and password are required",
			},
		},
		{
			name: "错误的凭据",
			requestBody: LoginRequest{
				Username: "wronguser",
				Password: "wrongpassword",
			},
			expectedStatus: http.StatusUnauthorized,
			expectedBody: map[string]interface{}{
				"status":  "error",
				"message": "Invalid credentials",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建测试路由
			router := gin.New()
			router.POST("/login", Login)

			// 创建测试请求
			jsonBody, _ := json.Marshal(tt.requestBody)
			req, _ := http.NewRequest("POST", "/login", bytes.NewBuffer(jsonBody))
			req.Header.Set("Content-Type", "application/json")

			// 创建响应记录器
			w := httptest.NewRecorder()

			// 执行请求
			router.ServeHTTP(w, req)

			// 断言状态码
			assert.Equal(t, tt.expectedStatus, w.Code)

			// 解析响应体
			var response map[string]interface{}
			err := json.Unmarshal(w.Body.Bytes(), &response)
			assert.NoError(t, err)

			// 断言响应体
			if tt.expectedStatus == http.StatusOK {
				// 对于成功的情况，我们只检查响应结构，不检查具体的 session token
				assert.Equal(t, "success", response["status"])
				assert.Equal(t, "", response["message"])
				data, ok := response["data"].(map[string]interface{})
				assert.True(t, ok)
				setCookie, ok := data["set-cookie"].(map[string]interface{})
				assert.True(t, ok)
				assert.NotEmpty(t, setCookie["session_token"])
				assert.Contains(t, strings.Join(w.Header().Values("Set-Cookie"), "\n"), "session_token=")
			} else {
				assert.Equal(t, tt.expectedBody, response)
			}
		})
	}
	// 清除测试数据
	accounts.DeleteAccountByUsername("testuser")
	accounts.DeleteAllSessions()
}

func TestSessionCookieIsAlwaysSecure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		requestURL string
		remoteAddr string
		forwarded  string
	}{
		{
			name:       "http",
			requestURL: "http://example.test/login",
		},
		{
			name:       "https",
			requestURL: "https://example.test/login",
		},
		{
			name:       "trusted reverse proxy",
			requestURL: "http://example.test/login",
			remoteAddr: "127.0.0.1:43000",
			forwarded:  "https",
		},
		{
			name:       "untrusted spoofed reverse proxy",
			requestURL: "http://example.test/login",
			remoteAddr: "203.0.113.10:43000",
			forwarded:  "https",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, tt.requestURL, nil)
			if tt.remoteAddr != "" {
				c.Request.RemoteAddr = tt.remoteAddr
			}
			if tt.forwarded != "" {
				c.Request.Header.Set("X-Forwarded-Proto", tt.forwarded)
			}

			setSessionCookie(c, "test-session", sessionCookieMaxAge)

			setCookie := strings.Join(w.Header().Values("Set-Cookie"), "\n")
			assert.Contains(t, setCookie, "Secure")
		})
	}
}

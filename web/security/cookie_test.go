package security

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSetSensitiveCookieUsesBrowserProtections(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, target := range []string{"http://example.test", "https://example.test"} {
		t.Run(target, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, target, nil)
			response := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(response)
			context.Request = request

			SetSensitiveCookie(context, "secret", "value", 60)
			cookies := response.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("cookies=%d, want 1", len(cookies))
			}
			cookie := cookies[0]
			if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
				t.Fatalf("cookie protections missing: %#v", cookie)
			}
		})
	}
}

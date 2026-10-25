package filemanager

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestUploadInitializationRequiresFreshTwoFactorBeforeFileControl(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, query := range []string{"", "?operation=init"} {
		t.Run(query, func(t *testing.T) {
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Params = gin.Params{{Key: "uuid", Value: "synthetic-agent"}}
			ctx.Request = httptest.NewRequest(http.MethodPost, "/upload"+query, strings.NewReader(`{"path":"/synthetic.txt","size":0}`))
			ctx.Request.Header.Set("Content-Type", "application/json")
			Upload(ctx)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("unauthorized upload reached file control: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

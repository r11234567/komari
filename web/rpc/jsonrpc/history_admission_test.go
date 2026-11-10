package jsonrpc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/security"
)

func TestCompatibilityBatchChargesEveryHistoricalMessage(t *testing.T) {
	controller := security.NewRateLimitController(true)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		api.SetPrincipal(c, rpc.NewUserPrincipal("synthetic-history-reader"))
		c.Set("uuid", "synthetic-history-reader")
		c.Next()
	}, controller.Middleware())
	router.POST("/api/rpc2", OnRpcRequest)
	batch := make([]map[string]any, 6)
	for i := range batch {
		batch[i] = map[string]any{"jsonrpc": "2.0", "id": i, "method": "public:queryMetrics", "params": map[string]any{}}
	}
	body, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/rpc2", bytes.NewReader(body)))
	var replies []rpc.JsonRpcResponse
	if err := json.Unmarshal(response.Body.Bytes(), &replies); err != nil || len(replies) != 6 {
		t.Fatalf("invalid batch response: %s", response.Body.String())
	}
	for i, reply := range replies {
		want := rpc.InvalidParams
		if i == 5 {
			want = rpc.Unavailable
		}
		if reply.Error == nil || reply.Error.Code != want {
			t.Fatalf("reply %d did not spend its own history budget: %+v", i, reply.Error)
		}
	}
}

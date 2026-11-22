package jsonrpc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestServePostEnforcesBodyAndBatchLimits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/rpc2", servePost)

	oversized := bytes.Repeat([]byte("a"), maxJSONRPCPostBytes+64)
	req := httptest.NewRequest(http.MethodPost, "/api/rpc2", bytes.NewReader(oversized))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized rpc2 status = %d, want 413", rec.Code)
	}

	batch := make([]map[string]any, maxJSONRPCBatchSize+1)
	for i := range batch {
		batch[i] = map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": "rpc.ping"}
	}
	rawBatch, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	batchReq := httptest.NewRequest(http.MethodPost, "/api/rpc2", bytes.NewReader(rawBatch))
	batchReq.Header.Set("Content-Type", "application/json")
	batchRec := httptest.NewRecorder()
	r.ServeHTTP(batchRec, batchReq)
	if batchRec.Code != http.StatusBadRequest || !strings.Contains(batchRec.Body.String(), "batch size exceeds limit") {
		t.Fatalf("oversized rpc2 batch status=%d body=%s", batchRec.Code, batchRec.Body.String())
	}
}

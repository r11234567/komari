package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestSensitive2FACodeCannotBeReplayed(t *testing.T) {
	now := time.Now()
	if !claimSensitive2FACode("user-a", "123456", now) {
		t.Fatal("first 2FA code use was rejected")
	}
	if claimSensitive2FACode("user-a", "123456", now.Add(time.Second)) {
		t.Fatal("replayed 2FA code was accepted")
	}
	if !claimSensitive2FACode("user-b", "123456", now.Add(time.Second)) {
		t.Fatal("another user's code was incorrectly treated as replay")
	}
}

func TestGet2FACodeBoundsAndPreservesBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Non-JSON multipart uploads must bypass body inspection cleanly while preserving all bytes.
	multipartPayload := bytes.Repeat([]byte("M"), 2<<20)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/upload", bytes.NewReader(multipartPayload))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=testboundary")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	if got := get2FACode(c); got != "" {
		t.Fatalf("multipart code = %q, want empty", got)
	}
	downstream, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(downstream) != len(multipartPayload) {
		t.Fatalf("multipart downstream len = %d, want %d", len(downstream), len(multipartPayload))
	}

	// Oversized JSON (>64 KiB) skips 2FA parsing without truncating downstream reads.
	oversizedJSON := []byte(`{"padding":"` + strings.Repeat("a", max2FABodyBytes+4096) + `","2fa_code":"999999"}`)
	reqLarge := httptest.NewRequest(http.MethodPost, "/api/admin/action", bytes.NewReader(oversizedJSON))
	reqLarge.Header.Set("Content-Type", "application/json; charset=utf-8")
	cLarge, _ := gin.CreateTestContext(httptest.NewRecorder())
	cLarge.Request = reqLarge
	if got := get2FACode(cLarge); got != "" {
		t.Fatalf("oversized JSON extracted code = %q, want empty", got)
	}
	downstreamLarge, err := io.ReadAll(cLarge.Request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downstreamLarge, oversizedJSON) {
		t.Fatalf("oversized JSON body corrupted: got %d bytes, want %d", len(downstreamLarge), len(oversizedJSON))
	}

	// Normal bounded JSON extracts 2fa_code and preserves the body.
	smallJSON := []byte(`{"2fa_code":"654321","other":"value"}`)
	reqSmall := httptest.NewRequest(http.MethodPost, "/api/admin/action", bytes.NewReader(smallJSON))
	reqSmall.Header.Set("Content-Type", "application/json")
	cSmall, _ := gin.CreateTestContext(httptest.NewRecorder())
	cSmall.Request = reqSmall
	if got := get2FACode(cSmall); got != "654321" {
		t.Fatalf("small JSON code = %q, want 654321", got)
	}
	downstreamSmall, err := io.ReadAll(cSmall.Request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downstreamSmall, smallJSON) {
		t.Fatalf("small JSON downstream mismatch: %q", downstreamSmall)
	}
}

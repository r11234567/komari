package public

import (
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"golang.org/x/image/font/basicfont"
)

func TestMJPEGConcurrentDrawingAndStreamLimit(t *testing.T) {
	// Verify font resource caps match hardened limits.
	if maxFontZipBytes != 10<<20 || maxFontFileBytes != 10<<20 {
		t.Fatalf("unexpected font limits: zip=%d file=%d", maxFontZipBytes, maxFontFileBytes)
	}
	if fontHTTPClient.Timeout != 15*1e9 {
		t.Fatalf("fontHTTPClient.Timeout = %s, want 15s", fontHTTPClient.Timeout)
	}

	// Concurrent drawing against a shared font.Face must be race-free under -race.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			img := image.NewRGBA(image.Rect(0, 0, 320, 80))
			for j := 0; j < 20; j++ {
				drawString(img, "Komari Node 01", 10, 20, color.White, basicfont.Face7x13)
				drawCenteredString(img, "Online 99.9%", 160, 50, color.White, basicfont.Face7x13)
			}
		}()
	}
	wg.Wait()

	// Saturate all maxMJPEGStreams slots and confirm the next caller gets 503 immediately.
	for i := 0; i < maxMJPEGStreams; i++ {
		mjpegStreamSem <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < maxMJPEGStreams; i++ {
			select {
			case <-mjpegStreamSem:
			default:
			}
		}
	})

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/mjpeg_live", nil)
	MjpegLiveHandler(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("MjpegLiveHandler when saturated returned %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

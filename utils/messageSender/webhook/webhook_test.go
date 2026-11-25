package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/utils/messageSender"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestWebhookUsesOutboundPolicyAndBoundsErrorResponse(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "settings.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	config.SetDb(db)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/error" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, strings.Repeat("a", 8192)+"tail")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	sender := &WebhookSender{}
	note := messageSender.Notification{}
	settings := map[string]any{"url": server.URL, "method": "POST"}
	if err := sender.Send(context.Background(), note, settings); err == nil || hits.Load() != 0 {
		t.Fatalf("default policy admitted loopback: %v, %d requests", err, hits.Load())
	}
	if err := config.Set(config.SSRFProtectionEnabledKey, false); err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(context.Background(), note, settings); err != nil || hits.Load() != 1 {
		t.Fatalf("explicit private webhook failed: %v, %d requests", err, hits.Load())
	}
	settings["url"] = server.URL + "/error"
	if err := sender.Send(context.Background(), note, settings); err == nil || len(err.Error()) > 4200 || !strings.Contains(err.Error(), "502") {
		t.Fatalf("unexpected error response: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sender.Send(ctx, note, settings); !errors.Is(err, context.Canceled) || hits.Load() != 2 {
		t.Fatalf("canceled request reached the webhook: %v, %d requests", err, hits.Load())
	}
	if err := config.Set(config.SSRFProtectionEnabledKey, true); err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(context.Background(), note, settings); err == nil || hits.Load() != 2 {
		t.Fatalf("protected request reused a private connection: %v, %d requests", err, hits.Load())
	}
}

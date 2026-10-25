package javascript

import (
	"context"
	"testing"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/utils/messageSender"
)

// Keep upgrades compatible with existing sendMessage scripts and the optional
// event callback. No real notification provider or user configuration is used.
func TestAuditLegacyJavaScriptNotificationArgumentsAndPromises(t *testing.T) {
	oldDataDir := DataDir
	DataDir = t.TempDir()
	t.Cleanup(func() { DataDir = oldDataDir })
	sender := &JavaScriptSender{}
	t.Cleanup(func() { _ = sender.Unload() })
	script := `
		async function sendMessage(message, title) {
			await Promise.resolve();
			if (message !== "synthetic message" || title !== "synthetic title") {
				throw new Error("legacy sendMessage argument order changed");
			}
			return true;
		}
	`
	for i := 0; i < 2; i++ {
		if err := sender.Send(context.Background(), messageSender.Notification{
			Message: "synthetic message", Title: "synthetic title",
		}, map[string]any{"script": script}); err != nil {
			t.Fatalf("legacy notification call %d: %v", i+1, err)
		}
	}
}

func TestAuditJavaScriptEventCallbackPreservesJSONFields(t *testing.T) {
	oldDataDir := DataDir
	DataDir = t.TempDir()
	t.Cleanup(func() { DataDir = oldDataDir })
	sender := &JavaScriptSender{}
	t.Cleanup(func() { _ = sender.Unload() })
	script := `
		function sendMessage() { throw new Error("sendEvent must take precedence"); }
		async function sendEvent(event) {
			await Promise.resolve();
			if (event.event !== "synthetic event" || event.message !== "synthetic payload") {
				throw new Error("event JSON fields were lost or renamed");
			}
			return true;
		}
	`
	if err := sender.Send(context.Background(), messageSender.Notification{
		Event: models.EventMessage{Event: "synthetic event", Message: "synthetic payload"},
	}, map[string]any{"script": script}); err != nil {
		t.Fatal(err)
	}
}

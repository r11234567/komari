package public

import (
	"strings"
	"testing"
)

func TestNativeLoginBridgePrecedesThemeAndIsIdempotent(t *testing.T) {
	document := `<html><HEAD><script src="theme.js"></script></HEAD><body></body></html>`
	got := injectNativeLoginBridge(document)
	if strings.Index(got, nativeLoginBridgeMarker) > strings.Index(got, `src="theme.js"`) {
		t.Fatal("native login bridge installed after theme")
	}
	if !strings.Contains(got, nativeLoginBridgeMarker) || injectNativeLoginBridge(got) != got {
		t.Fatal("missing or duplicate login bridge")
	}
}

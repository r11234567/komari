package public

import (
	_ "embed"
	"strings"
)

//go:embed native_login_bridge.js
var nativeLoginBridgeJS string

const nativeLoginBridgeMarker = "data-komari-native-login"

func injectNativeLoginBridge(document string) string {
	if strings.Contains(document, nativeLoginBridgeMarker) {
		return document
	}
	script := "<script " + nativeLoginBridgeMarker + ">" + nativeLoginBridgeJS + "</script>"
	// Install the legacy API adapter before theme entry scripts. Dialog opening
	// itself never navigates; only successful password verification hands off.
	if loc := headOpenPattern.FindStringIndex(document); loc != nil {
		return document[:loc[1]] + script + document[loc[1]:]
	}
	return script + document
}

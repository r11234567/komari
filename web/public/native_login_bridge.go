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
	// Install before the theme's bundle, including synchronous entry scripts.
	if loc := headOpenPattern.FindStringIndex(document); loc != nil {
		return document[:loc[1]] + script + document[loc[1]:]
	}
	return script + document
}

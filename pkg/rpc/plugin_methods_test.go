package rpc

import "testing"

func TestPluginMethodMarkerLifecycle(t *testing.T) {
	const method = "plugin:marker-test"
	MarkPluginMethod(method)
	if !IsPluginMethod(method) {
		t.Fatal("marked method is not reported as a plugin method")
	}
	Unregister(method)
	if IsPluginMethod(method) {
		t.Fatal("unregister must clear the plugin method marker")
	}
}

package jsonrpc

import (
	"testing"

	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestRPC2CompatibilitySurface(t *testing.T) {
	allowed := []string{
		"public:getMe",
		"public:getNodesInformation",
		"public:getPublicSettings",
		"public:getVersion",
		"public:getClientRecentRecords",
		"public:getRecordsByUUID",
		"public:getPingRecords",
		"public:getPublicPingTasks",
		"public:listMetricDefinitions",
		"public:queryMetrics",
		"public:getPingMetricStats",
		"public:recordVisitorEvent",
		"common:getNodes",
		"common:getNodesLatestStatus",
	}
	for _, method := range allowed {
		if !rpc2TransportAllowed(method) {
			t.Errorf("%s must stay available for third-party dashboards", method)
		}
	}

	denied := []string{
		"admin:getSettings",
		"admin:setXtermjsSettings",
		"admin:fileList",
		"admin:getClipboard",
		"admin:listClipboard",
		"client:taskResult",
		"common:getVersion",
		"rpc.ping",
	}
	for _, method := range denied {
		if rpc2TransportAllowed(method) {
			t.Errorf("%s must not be reachable through /api/rpc2", method)
		}
	}

	const pluginMethod = "plugin:compat-test"
	rpc.MarkPluginMethod(pluginMethod)
	t.Cleanup(func() { rpc.Unregister(pluginMethod) })
	if !rpc2TransportAllowed(pluginMethod) {
		t.Fatal("plugin-owned methods must stay available")
	}
}

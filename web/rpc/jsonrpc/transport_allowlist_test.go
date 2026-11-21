package jsonrpc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
	v1 "github.com/komari-monitor/komari/protocol/v1"
	agent_runtime "github.com/komari-monitor/komari/web/agent"
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
		"common:getNodes",
		"common:getNodesLatestStatus",
		"common:getVersion",
		"common:getRecords",
		"common:getNodeRecentStatus",
		"common:getPublicInfo",
		"common:getMe",
		"rpc.ping",
		"rpc.version",
	}
	if len(rpc2CompatibilityMethods) != len(allowed) {
		t.Fatalf("rpc2CompatibilityMethods size = %d, want %d", len(rpc2CompatibilityMethods), len(allowed))
	}
	for _, method := range allowed {
		if !rpc2TransportAllowed(method) {
			t.Errorf("%s must stay available for third-party dashboards", method)
		}
	}

	denied := []string{
		"admin:listTrustedCredentialProviders", "admin:beginTrustedCredentialVerification",
		"admin:completeTrustedCredentialVerification", "admin:cancelTrustedCredentialVerification",
		"admin:getSettings",
		"admin:listPlugins", "admin:setPluginEnabled", "admin:getPluginLogs",
		"admin:deletePlugin", "admin:getPluginConfiguration", "admin:setPluginConfiguration",
		"admin:setXtermjsSettings",
		"admin:fileList",
		"admin:getClipboard",
		"admin:listClipboard",
		"client:taskResult",
		"public:recordVisitorEvent",
		"rpc.methods",
		"rpc.help",
	}
	for _, method := range denied {
		if rpc2TransportAllowed(method) {
			t.Errorf("%s must not be reachable through /api/rpc2", method)
		}
	}
	for _, method := range []string{"admin:plugin-disguise", "client:plugin-disguise", "public:plugin-disguise", "common:plugin-disguise", "plugin:compat-test"} {
		rpc.MarkPluginMethod(method)
		if rpc2TransportAllowed(method) {
			t.Errorf("plugin method must not be reachable through /api/rpc2: %s", method)
		}
		rpc.Unregister(method)
	}
}

func TestPublicViewerProjectionIgnoresAdminPrincipal(t *testing.T) {
	db := dbcore.GetDBInstance()
	visibleID := "vis-" + uuid.NewString()[:8]
	hiddenID := "hid-" + uuid.NewString()[:8]
	visibleNode := models.Client{
		UUID:                   visibleID,
		Token:                  "tok-" + visibleID,
		Name:                   "Visible Node",
		IPv4:                   "198.51.100.42",
		IPv6:                   "2001:db8::42",
		Remark:                 "private operator note",
		Version:                "1.9.9-secret",
		RegionOverride:         "US",
		RemoteControlProtected: true,
		Hidden:                 false,
	}
	hiddenNode := models.Client{
		UUID:   hiddenID,
		Token:  "tok-" + hiddenID,
		Name:   "Hidden Node",
		IPv4:   "198.51.100.99",
		Hidden: true,
	}
	if err := db.Create(&visibleNode).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&hiddenNode).Error; err != nil {
		t.Fatal(err)
	}
	agent_runtime.RecordReport(v1.Report{UUID: visibleID, UpdatedAt: time.Now().UTC(), CPU: v1.CPUReport{Usage: 11}})
	agent_runtime.RecordReport(v1.Report{UUID: hiddenID, UpdatedAt: time.Now().UTC(), CPU: v1.CPUReport{Usage: 77}})
	t.Cleanup(func() {
		agent_runtime.DeleteLatestReport(visibleID)
		agent_runtime.DeleteLatestReport(hiddenID)
		db.Where("uuid IN ?", []string{visibleID, hiddenID}).Delete(&models.Client{})
		_ = config.Set(config.SendIpAddrToGuestKey, false)
	})

	_ = config.Set(config.SendIpAddrToGuestKey, true)
	adminCtx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{
		Principal:  rpc.NewUserPrincipal("admin-user"),
		Permission: rpc.RoleAdmin,
		UserUUID:   "admin-user",
	})

	rawNodes, rpcErr := getNodes(adminCtx, &rpc.JsonRpcRequest{Params: map[string]any{}})
	if rpcErr != nil {
		t.Fatalf("common:getNodes failed: %v", rpcErr)
	}
	nodeMap := rawNodes.(map[string]models.Client)
	if _, leaked := nodeMap[hiddenID]; leaked {
		t.Fatal("common:getNodes leaked Hidden node when called with admin principal")
	}
	gotVis, ok := nodeMap[visibleID]
	if !ok {
		t.Fatal("common:getNodes omitted visible node")
	}
	if gotVis.IPv4 != "198.*.*.*" || gotVis.IPv6 != "2001:*:*:*:*:*:*:*" || gotVis.Remark != "" || gotVis.Version != "" || gotVis.Token != "" || gotVis.RegionOverride != "" || gotVis.RemoteControlProtected {
		t.Fatalf("common:getNodes leaked private fields: %+v", gotVis)
	}

	rawPubNodes, rpcErr := publicGetNodesInformation(adminCtx, &rpc.JsonRpcRequest{})
	if rpcErr != nil {
		t.Fatalf("public:getNodesInformation failed: %v", rpcErr)
	}
	for _, n := range rawPubNodes.([]models.Client) {
		if n.UUID == hiddenID {
			t.Fatal("public:getNodesInformation leaked Hidden node")
		}
		if n.UUID == visibleID && (n.IPv4 != "" || n.IPv6 != "" || n.Remark != "" || n.Version != "" || n.Token != "" || n.RegionOverride != "" || n.RemoteControlProtected) {
			t.Fatalf("public:getNodesInformation leaked private fields: %+v", n)
		}
	}

	if _, rpcErr := getNodesLatestStatus(adminCtx, &rpc.JsonRpcRequest{Params: map[string]any{"uuid": hiddenID}}); rpcErr == nil {
		t.Fatal("common:getNodesLatestStatus allowed querying Hidden node with admin principal")
	}
	if _, rpcErr := getNodeRecentStatus(adminCtx, &rpc.JsonRpcRequest{Params: map[string]any{"uuid": hiddenID}}); rpcErr == nil {
		t.Fatal("common:getNodeRecentStatus allowed querying Hidden node with admin principal")
	}
	entityIDs, rpcErr := publicMetricEntityIDs(adminCtx, []string{visibleID, hiddenID, "nonexistent-node-id"})
	if rpcErr != nil {
		t.Fatalf("publicMetricEntityIDs failed: %v", rpcErr)
	}
	if len(entityIDs) != 1 || entityIDs[0] != visibleID {
		t.Fatalf("publicMetricEntityIDs = %v, want only [%s]", entityIDs, visibleID)
	}
}

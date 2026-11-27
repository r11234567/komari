package connectapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/plugin"
	"github.com/komari-monitor/komari/pkg/rpc"
	pluginv1 "github.com/r11234567/komari-proto/gen/go/komari/plugin/v1"
	pluginv1connect "github.com/r11234567/komari-proto/gen/go/komari/plugin/v1/pluginv1connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestPluginConfigurationNodeAndPingSelectorsThroughConnect(t *testing.T) {
	ctx, _, _ := actionFixture(t)
	short := "media-ai-fixture"
	oldDir := plugin.DataDir
	plugin.DataDir = t.TempDir()
	t.Cleanup(func() { plugin.DataDir = oldDir })
	dir := filepath.Join(plugin.DataDir, short)
	require.NoError(t, os.Mkdir(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "komari-plugin.json"), []byte(`{
		"short":"media-ai-fixture", "name":"Media and AI test fixture", "version":"1",
		"configuration":{"type":"managed","data":[
			{"key":"nodes","type":"nodes"},
			{"key":"pings","type":"pingtasks"},
			{"key":"label","type":"string","default":"Original"}
		]}
	}`), 0600))

	db := dbcore.GetDBInstance()
	nodeID := uuid.NewString()
	require.NoError(t, db.Create(&models.Client{UUID: nodeID, Name: "Plugin fixture node"}).Error)
	testTask := models.PingTask{Name: "Plugin fixture ping", Target: "example.test"}
	require.NoError(t, db.Create(&testTask).Error)
	t.Cleanup(func() {
		db.Delete(&models.PingTask{}, testTask.Id)
		db.Where("uuid = ?", nodeID).Delete(&models.Client{})
		db.Where("short = ?", short).Delete(&models.PluginConfiguration{})
	})

	_, handler := pluginv1connect.NewPluginServiceHandler(&pluginService{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(rpc.NewContextWithMeta(r.Context(), rpc.MetaFromContext(ctx))))
	}))
	defer server.Close()
	client := pluginv1connect.NewPluginServiceClient(server.Client(), server.URL)
	read := func() map[string]any {
		t.Helper()
		res, err := client.GetPluginConfiguration(ctx, connect.NewRequest(&pluginv1.GetPluginConfigurationRequest{ShortName: short}))
		require.NoError(t, err)
		return res.Msg.Values.AsMap()
	}
	initial := read()
	require.Equal(t, []any{}, initial["nodes"])
	require.Equal(t, []any{}, initial["pings"])
	require.Equal(t, "Original", initial["label"])

	require.NoError(t, plugin.SaveConfiguration(short, map[string]any{
		"nodes": fmt.Sprintf("[%q]", nodeID),
		"pings": fmt.Sprintf("[%d]", testTask.Id),
	}))
	require.Equal(t, map[string]any{
		"nodes": []any{nodeID},
		"pings": []any{float64(testTask.Id)},
		"label": "Original",
	}, read())

	values, err := structpb.NewStruct(map[string]any{
		"nodes": []any{nodeID, uuid.NewString()},
		"pings": []any{float64(testTask.Id), float64(testTask.Id) + 1_000_000},
		"label": "Updated",
	})
	require.NoError(t, err)
	_, err = client.SetPluginConfiguration(ctx, connect.NewRequest(&pluginv1.SetPluginConfigurationRequest{ShortName: short, Values: values}))
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"nodes": []any{nodeID},
		"pings": []any{float64(testTask.Id)},
		"label": "Updated",
	}, read())
}

package jsonrpc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestSampleEvenly(t *testing.T) {
	input := []int{0, 1, 2, 3, 4}
	tests := []struct {
		name  string
		count int
		want  []int
	}{
		{name: "empty", count: 0, want: []int{}},
		{name: "latest only", count: 1, want: []int{4}},
		{name: "even selection", count: 3, want: []int{0, 2, 4}},
		{name: "all", count: len(input), want: input},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sampleEvenly(input, test.count); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("sampleEvenly(%v, %d) = %v, want %v", input, test.count, got, test.want)
			}
		})
	}
}

func TestAllocateTargetsSupportsTypedKeys(t *testing.T) {
	groups := []allocationGroup[uint]{
		{key: 7, length: 6},
		{key: 9, length: 4},
	}
	got := allocateTargets(groups, 5)
	if got[7] != 3 || got[9] != 2 {
		t.Fatalf("allocateTargets() = %v, want map[7:3 9:2]", got)
	}
}

func TestClampCompatibilityRecordMaxCountEnforcesCeiling(t *testing.T) {
	for _, input := range []int{-1, -100, 0, 4001, 100_000} {
		if got := clampCompatibilityRecordMaxCount(input); got != maxCompatibilityRecordPoints {
			t.Fatalf("clampCompatibilityRecordMaxCount(%d) = %d, want %d", input, got, maxCompatibilityRecordPoints)
		}
	}
	for _, input := range []int{1, 120, 4000} {
		if got := clampCompatibilityRecordMaxCount(input); got != input {
			t.Fatalf("clampCompatibilityRecordMaxCount(%d) = %d, want %d", input, got, input)
		}
	}
}

func TestFilterVisiblePingTaskClientsStripsHiddenUUIDs(t *testing.T) {
	hidden := map[string]bool{
		"node-hidden-1": true,
		"node-hidden-2": true,
	}
	got := filterVisiblePingTaskClients([]string{"node-visible-a", "node-hidden-1", "", "node-visible-b", "node-hidden-2"}, hidden)
	want := []string{"node-visible-a", "node-visible-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterVisiblePingTaskClients() = %v, want %v", got, want)
	}
	empty := filterVisiblePingTaskClients([]string{"node-hidden-1"}, hidden)
	if empty == nil || len(empty) != 0 {
		t.Fatalf("expected non-nil empty slice when all clients are hidden, got %#v", empty)
	}
}

func TestCommonGetRecordsPingStatisticsInvariantToMaxCount(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	db := dbcore.GetDBInstance()
	if err := config.Set(metricstore.MetricDBDSNKey, filepath.Join(tmpDir, "metrics.db")); err != nil {
		t.Fatalf("set metric_db_dsn: %v", err)
	}
	if err := metricstore.Reload(ctx); err != nil {
		t.Fatalf("metricstore.Reload: %v", err)
	}

	clientUUID := "node-ping-stats-1"
	_ = db.Where("uuid = ?", clientUUID).Delete(&models.Client{}).Error
	if err := db.Create(&models.Client{UUID: clientUUID, Name: "Ping Node", Hidden: false}).Error; err != nil {
		t.Fatalf("create client: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Where("uuid = ?", clientUUID).Delete(&models.Client{}).Error
	})
	task := models.PingTask{
		Name:     "ICMP Test",
		Clients:  models.StringArray{clientUUID},
		Type:     "icmp",
		Target:   "198.51.100.1",
		Interval: 60,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create ping task: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Where("id = ?", task.Id).Delete(&models.PingTask{}).Error
	})

	end := time.Now().UTC().Truncate(time.Minute)
	start := end.Add(-60 * time.Minute)
	for i := 0; i < 60; i++ {
		val := 20 + (i*7)%80
		if i%10 == 3 {
			val = -1 // packet loss
		}
		if err := metricstore.WritePingRecord(ctx, models.PingRecord{
			Client: clientUUID,
			TaskId: task.Id,
			Time:   start.Add(time.Duration(i+1) * time.Minute),
			Value:  val,
		}); err != nil {
			t.Fatalf("WritePingRecord(%d): %v", i, err)
		}
	}

	callPingRecords := func(maxCount int) map[string]any {
		t.Helper()
		req := rpc.NewRequest(1, "common:getRecords", map[string]any{
			"type":     "ping",
			"uuid":     clientUUID,
			"task_id":  int(task.Id),
			"start":    start.Format(time.RFC3339),
			"end":      end.Format(time.RFC3339),
			"maxCount": maxCount,
		})
		res, rpcErr := getRecords(ctx, req)
		if rpcErr != nil {
			t.Fatalf("getRecords(maxCount=%d) rpcErr: %+v", maxCount, rpcErr)
		}
		raw, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal response: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		return out
	}

	small := callPingRecords(10)
	large := callPingRecords(500)

	smallRecs := small["records"].([]any)
	largeRecs := large["records"].([]any)
	if len(smallRecs) != 10 {
		t.Fatalf("small maxCount=10 returned %d records, want 10", len(smallRecs))
	}
	if len(largeRecs) != 60 {
		t.Fatalf("large maxCount=500 returned %d records, want 60", len(largeRecs))
	}
	if !reflect.DeepEqual(small["basic_info"], large["basic_info"]) {
		t.Fatalf("BasicInfo changed with maxCount:\nsmall=%#v\nlarge=%#v", small["basic_info"], large["basic_info"])
	}
	if !reflect.DeepEqual(small["tasks"], large["tasks"]) {
		t.Fatalf("Tasks summary statistics changed with maxCount:\nsmall=%#v\nlarge=%#v", small["tasks"], large["tasks"])
	}
}

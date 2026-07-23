package jsonrpc

import (
	"context"
	"fmt"

	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
)

const ltsMaxMetricRetentionDays = 36500

type ltsMetricRetentionChange struct {
	Name          string `json:"name"`
	RetentionDays int    `json:"retention_days"`
}

func init() {
	reg("updateMetricDefinition", adminUpdateMetricDefinition, "Update 1.2.5 LTS metric retention")
}

func adminUpdateMetricDefinition(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params ltsMetricRetentionChange
	if err := req.BindParams(&params); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid metric retention update: "+err.Error(), nil)
	}
	source, ok := ltsMetricDefinitionByName(params.Name)
	if !ok {
		return nil, rpc.MakeError(rpc.InvalidParams, "Unknown metric: "+params.Name, nil)
	}
	if params.RetentionDays < 0 || params.RetentionDays > ltsMaxMetricRetentionDays {
		return nil, rpc.MakeError(rpc.InvalidParams,
			fmt.Sprintf("retention_days must be between 0 and %d", ltsMaxMetricRetentionDays), nil)
	}

	ltsMetricRetentionMu.Lock()
	defer ltsMetricRetentionMu.Unlock()
	retention := ltsMetricRetentionDaysLocked()
	retention[params.Name] = params.RetentionDays
	resourceDays, pingDays := ltsPhysicalRetentionDays(retention)
	taskResultPreserveTime := ltsTaskResultPreserveTimeLocked()
	err := config.SetMany(map[string]any{
		ltsMetricRetentionConfigKey:      retention,
		config.RecordPreserveTimeKey:     resourceDays * 24,
		config.PingRecordPreserveTimeKey: pingDays * 24,
		config.TaskResultPreserveTimeKey: taskResultPreserveTime,
	})
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to save metric retention: "+err.Error(), nil)
	}

	actor, ip := auditActor(ctx)
	auditlog.Log(ip, actor,
		fmt.Sprintf("updated metric retention: %s=%d days", params.Name, params.RetentionDays), "info")

	for _, definition := range ltsMetricDefinitions {
		if definition.key == params.Name {
			return ltsMetricDefinition{
				Name: definition.key, Description: definition.description, Type: "gauge",
				Unit: definition.unit, RetentionDays: float64(params.RetentionDays),
			}, nil
		}
	}
	return nil, rpc.MakeError(rpc.InternalError, "Metric definition disappeared", map[string]any{"source": source})
}

func ltsTaskResultPreserveTimeLocked() int {
	hours, err := config.GetAs[int](config.TaskResultPreserveTimeKey)
	if err == nil {
		return hours
	}
	settings, settingsErr := config.GetManyAs[config.Settings]()
	if settingsErr == nil && settings != nil {
		return settings.RecordPreserveTime
	}
	return 720
}

func ltsPhysicalRetentionDays(retention map[string]int) (resourceDays, pingDays int) {
	for _, definition := range ltsMetricDefinitions {
		days := retention[definition.key]
		if definition.source == "ping" {
			pingDays = max(pingDays, days)
			continue
		}
		resourceDays = max(resourceDays, days)
	}
	return resourceDays, pingDays
}

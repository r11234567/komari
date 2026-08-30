package jsonrpc

import (
	"context"
	"strconv"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/trafficledger"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func init() {
	RegisterWithGroupAndMeta("getClientDailyTraffic", rpc.RoleAdmin, adminGetClientDailyTraffic, &rpc.MethodMeta{
		Name: "admin:getClientDailyTraffic", Summary: "Get one client's exact daily traffic history",
	})
}

type clientDailyTrafficPoint struct {
	Day      string `json:"day"`
	Up       int64  `json:"up"`
	Down     int64  `json:"down"`
	Billable int64  `json:"billable"`
}

func adminGetClientDailyTraffic(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params struct {
		UUID string `json:"uuid"`
		Days string `json:"days"`
	}
	if err := req.BindParams(&params); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid traffic request: "+err.Error(), nil)
	}
	days := trafficledger.DashboardHistoryDays
	if params.Days != "" {
		parsed, err := strconv.Atoi(params.Days)
		if err != nil || parsed < 1 || parsed > trafficledger.DashboardHistoryDays {
			return nil, rpc.MakeError(rpc.InvalidParams, "days must be between 1 and 30", nil)
		}
		days = parsed
	}
	db := dbcore.GetDBInstance()
	var client models.Client
	if err := db.WithContext(ctx).Select("uuid", "name", "traffic_limit_type").Where("uuid = ?", params.UUID).First(&client).Error; err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Unknown server", nil)
	}
	now := time.Now().UTC()
	today := trafficledger.BeijingDay(now)
	start := today.AddDate(0, 0, -(days - 1))
	var rows []models.TrafficDailyLedger
	if err := db.WithContext(ctx).Select("day", "up_bytes", "down_bytes").
		Where("client = ? AND day >= ? AND day < ?", client.UUID, start.Format(time.DateOnly), today.Format(time.DateOnly)).
		Order("day ASC").Find(&rows).Error; err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to read daily traffic: "+err.Error(), nil)
	}
	adjustments, err := trafficledger.DailyAdjustments(ctx, db, start, today.AddDate(0, 0, 1))
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to read traffic calibration: "+err.Error(), nil)
	}
	todayUsage, err := trafficledger.MetricUsage(ctx, client.UUID, today.UTC(), now)
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to read today's traffic: "+err.Error(), nil)
	}
	usageByDay := make(map[string]trafficledger.Usage, len(rows)+1)
	for _, row := range rows {
		usageByDay[row.Day] = trafficledger.ApplyAdjustment(
			trafficledger.Usage{Up: row.UpBytes, Down: row.DownBytes},
			adjustments[client.UUID+"\x00"+row.Day],
		)
	}
	todayKey := today.Format(time.DateOnly)
	usageByDay[todayKey] = trafficledger.ApplyAdjustment(todayUsage, adjustments[client.UUID+"\x00"+todayKey])
	result := make([]clientDailyTrafficPoint, 0, days)
	for day := start; !day.After(today); day = day.AddDate(0, 0, 1) {
		key := day.Format(time.DateOnly)
		usage := usageByDay[key]
		result = append(result, clientDailyTrafficPoint{
			Day: key, Up: usage.Up, Down: usage.Down,
			Billable: trafficledger.BillableUsage(client.TrafficLimitType, usage.Up, usage.Down),
		})
	}
	return map[string]any{
		"client": client.UUID, "name": client.Name,
		"timezone": "Asia/Shanghai", "days": result,
	}, nil
}

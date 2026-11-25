package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/trafficledger"
	"github.com/komari-monitor/komari/pkg/trafficreset"
	"github.com/komari-monitor/komari/web/api"
	"gorm.io/gorm"
)

// The administrator dialog loads the authoritative policy when opened. This
// avoids inferring settings from the public node summary's effective quota.
func GetTrafficPolicy(c *gin.Context) {
	client, err := findTrafficCalibrationClient(c.Param("uuid"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		api.RespondError(c, http.StatusNotFound, "服务器不存在")
		return
	}
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "读取流量设置失败："+err.Error())
		return
	}
	now := time.Now().UTC()
	ctx, db := c.Request.Context(), dbcore.GetDBInstance()
	result := trafficPolicy(client, now)
	schedule, err := trafficledger.TrafficScheduleAt(ctx, db, client, now)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "读取流量账期失败")
		return
	}
	if client.TrafficPolicyManaged {
		result = trafficPolicyFields(&schedule.Day, schedule, now)
	}
	result["versioned_policy"] = metricstore.TelemetryEnabled()
	if result["versioned_policy"] == true {
		if err := addTrafficPolicyStatus(ctx, db, client, now, result); err != nil {
			api.RespondError(c, http.StatusInternalServerError, "读取流量入账状态失败")
			return
		}
	}
	api.RespondSuccess(c, result)
}

func trafficPolicy(client models.Client, now time.Time) gin.H {
	schedule := trafficreset.FromFields(client.TrafficResetDay, client.TrafficResetTime, client.TrafficResetTimezone)
	return trafficPolicyFields(client.TrafficResetDay, schedule, now)
}

func trafficPolicyFields(day *int, schedule trafficreset.Schedule, now time.Time) gin.H {
	return gin.H{"traffic_reset_day": day, "traffic_reset_time": trafficreset.FormatClock(schedule.Hour, schedule.Minute, schedule.Second), "traffic_reset_timezone": schedule.Timezone, "next_reset": schedule.FormatNext(now)}
}

func addTrafficPolicyStatus(ctx context.Context, db *gorm.DB, client models.Client, now time.Time, result gin.H) error {
	pending, err := trafficledger.PendingTrafficPolicy(ctx, db, client, now)
	if err != nil {
		return err
	}
	if pending != nil {
		result["pending"] = gin.H{"day": pending.Day, "time": pending.Clock, "timezone": pending.Timezone, "effective_at": time.Unix(0, pending.Effective).UTC()}
	}
	var state models.TelemetryTrafficState
	if err = db.WithContext(ctx).Where("client = ?", client.UUID).Take(&state).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if state.LastObserved > 0 {
		result["accounted_through"] = time.Unix(0, state.LastObserved).UTC()
	}
	var gaps []models.TelemetryTrafficGap
	if err := db.WithContext(ctx).Where("client = ?", client.UUID).Order("end DESC").Limit(10).Find(&gaps).Error; err != nil {
		return err
	}
	entries := make([]gin.H, 0, len(gaps))
	for _, gap := range gaps {
		entries = append(entries, gin.H{"start": time.Unix(0, gap.Start).UTC(), "end": time.Unix(0, gap.End).UTC(), "kind": gap.Kind, "up_bytes": gap.UpBytes, "down_bytes": gap.DownBytes})
	}
	result["gaps"] = entries
	if store, release, err := metricstore.AcquireStore(ctx); err == nil {
		defer release()
		if t := store.Telemetry(); t != nil {
			if at, err := t.LatestTrafficSample(ctx, client.UUID); err == nil && !at.IsZero() {
				result["received_through"] = at
			}
		}
	}
	return nil
}

func UpdateTrafficPolicy(c *gin.Context) {
	var request struct {
		Day      *int   `json:"traffic_reset_day"`
		Clock    string `json:"traffic_reset_time"`
		Timezone string `json:"traffic_reset_timezone"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.Day == nil || *request.Day < 0 || *request.Day > 31 {
		api.RespondError(c, http.StatusBadRequest, "请选择 0 到 31 日的重置日期")
		return
	}
	clock, err := trafficreset.NormalizeClock(request.Clock)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	zone, err := trafficreset.NormalizeTimezone(request.Timezone)
	if err != nil || zone == "Local" || len(zone) > 64 {
		api.RespondError(c, http.StatusBadRequest, "时区无效")
		return
	}
	id := c.Param("uuid")
	if metricstore.TelemetryEnabled() {
		err = trafficledger.SaveTrafficPolicy(c.Request.Context(), dbcore.GetDBInstance(), id, *request.Day, clock, zone, time.Now().UTC())
	} else {
		err = clients.SaveClient(map[string]any{"uuid": id, "traffic_reset_day": *request.Day, "traffic_reset_time": clock, "traffic_reset_timezone": zone})
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		api.RespondError(c, http.StatusNotFound, "服务器不存在")
		return
	}
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "保存流量账期失败")
		return
	}
	api.RespondSuccess(c, nil)
}

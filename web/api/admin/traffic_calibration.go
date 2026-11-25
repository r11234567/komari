package admin

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/trafficledger"
	"github.com/komari-monitor/komari/web/api"
	"gorm.io/gorm"
)

type trafficCalibrationRequest struct {
	TargetUp     *int64     `json:"target_up"`
	TargetDown   *int64     `json:"target_down"`
	TargetTotal  *int64     `json:"target_total"`
	CountingType string     `json:"counting_type"`
	Cycle        string     `json:"cycle"`
	Note         string     `json:"note"`
	Cutoff       *time.Time `json:"cutoff"`
}

func findTrafficCalibrationClient(uuid string) (models.Client, error) {
	var client models.Client
	err := dbcore.GetDBInstance().
		Select("uuid", "name", "traffic_limit_type", "traffic_reset_day", "traffic_reset_time", "traffic_reset_timezone", "traffic_policy_managed").
		Where("uuid = ?", uuid).
		First(&client).Error
	return client, err
}

func GetTrafficCalibration(c *gin.Context) {
	client, err := findTrafficCalibrationClient(c.Param("uuid"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		api.RespondError(c, http.StatusNotFound, "服务器不存在")
		return
	}
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "读取服务器失败："+err.Error())
		return
	}
	now := time.Now().UTC()
	schedule, err := trafficledger.TrafficScheduleAt(c.Request.Context(), dbcore.GetDBInstance(), client, now)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "读取流量周期失败")
		return
	}
	if !schedule.Active() {
		api.RespondSuccess(c, gin.H{
			"available": false,
			"client":    client.UUID,
			"reason":    "请先设置月度账期",
		})
		return
	}
	snapshot, err := trafficledger.LoadCalibrationSnapshot(
		c.Request.Context(), dbcore.GetDBInstance(), client, time.Now().UTC(),
	)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "读取流量校准信息失败："+err.Error())
		return
	}
	api.RespondSuccess(c, gin.H{"available": true, "snapshot": snapshot, "counting_type": trafficledger.NormalizedCountingType(client.TrafficLimitType), "cutoff_enabled": metricstore.TelemetryEnabled()})
}

func UpdateTrafficCalibration(c *gin.Context) {
	var request trafficCalibrationRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		api.RespondError(c, http.StatusBadRequest, "流量校准参数无效："+err.Error())
		return
	}
	if err := validateCalibrationRequest(request); err != nil {
		api.RespondError(c, http.StatusBadRequest, "请输入非负总量，或同时输入非负的上传和下载；两种方式不能混用")
		return
	}
	client, err := findTrafficCalibrationClient(c.Param("uuid"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		api.RespondError(c, http.StatusNotFound, "服务器不存在")
		return
	}
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "读取服务器失败："+err.Error())
		return
	}
	actor, _ := c.Get("uuid")
	now := time.Now().UTC()
	schedule, cycleErr := trafficledger.TrafficScheduleAt(c.Request.Context(), dbcore.GetDBInstance(), client, now)
	cycle := schedule.CycleKey(now)
	if cycleErr != nil || cycle == "" || (request.Cycle != "" && request.Cycle != cycle) {
		api.RespondError(c, http.StatusConflict, "流量周期已变化，请重新打开校准窗口")
		return
	}
	if len([]rune(request.Note)) > 300 {
		api.RespondError(c, http.StatusBadRequest, "校准说明最多 300 字")
		return
	}
	countingType := trafficledger.NormalizedCountingType(client.TrafficLimitType)
	if request.TargetTotal != nil && request.CountingType != "" && request.CountingType != countingType {
		api.RespondError(c, http.StatusConflict, "计费口径已变化，请重新打开校准窗口")
		return
	}
	var snapshot trafficledger.CalibrationSnapshot
	mode := "directions"
	if request.Cutoff != nil || (client.TrafficPolicyManaged && metricstore.TelemetryEnabled()) {
		if !metricstore.TelemetryEnabled() {
			api.RespondError(c, http.StatusConflict, "当前账本不支持按时间校准")
			return
		}
		cutoff := now.Truncate(time.Minute)
		if request.Cutoff != nil {
			cutoff = *request.Cutoff
		}
		var target trafficledger.Usage
		if request.TargetTotal == nil {
			target = trafficledger.Usage{Up: *request.TargetUp, Down: *request.TargetDown}
		} else {
			mode = "total:" + countingType
		}
		snapshot, err = trafficledger.SaveTrafficCutoff(c.Request.Context(), dbcore.GetDBInstance(), client, cycle, cutoff, target, request.TargetTotal, countingType, fmt.Sprint(actor), strings.TrimSpace(request.Note), now)
	} else if request.TargetTotal != nil {
		mode = "total:" + countingType
		snapshot, err = trafficledger.CalibrateBillableCycle(c.Request.Context(), dbcore.GetDBInstance(), client, *request.TargetTotal, fmt.Sprint(actor), now)
	} else {
		snapshot, err = trafficledger.CalibrateCurrentCycle(c.Request.Context(), dbcore.GetDBInstance(), client, trafficledger.Usage{Up: *request.TargetUp, Down: *request.TargetDown}, fmt.Sprint(actor), now)
	}
	if err != nil {
		if errors.Is(err, trafficledger.ErrCalibrationCatchingUp) {
			c.Header("Retry-After", "1")
			api.RespondError(c, http.StatusServiceUnavailable, "流量账本正在追赶已接收的报告，请稍后重试校准")
			return
		}
		api.RespondError(c, http.StatusBadRequest, "保存流量校准失败："+err.Error())
		return
	}
	auditlog.Log(
		c.ClientIP(),
		fmt.Sprint(actor),
		fmt.Sprintf("calibrated traffic for client %s cycle=%s mode=%s to up=%d down=%d note=%q", client.UUID, cycle, mode, snapshot.Effective.Up, snapshot.Effective.Down, strings.TrimSpace(request.Note)),
		"warn",
	)
	api.RespondSuccess(c, gin.H{"available": true, "snapshot": snapshot, "counting_type": countingType, "cutoff_enabled": metricstore.TelemetryEnabled()})
}

func validateCalibrationRequest(request trafficCalibrationRequest) error {
	if request.TargetTotal != nil {
		if *request.TargetTotal < 0 || request.TargetUp != nil || request.TargetDown != nil {
			return fmt.Errorf("provide either a nonnegative total or both directions")
		}
		return nil
	}
	if request.TargetUp == nil || request.TargetDown == nil || *request.TargetUp < 0 || *request.TargetDown < 0 {
		return fmt.Errorf("both nonnegative directions are required")
	}
	return nil
}

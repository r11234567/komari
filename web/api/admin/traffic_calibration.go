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
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/trafficledger"
	"github.com/komari-monitor/komari/web/api"
	"gorm.io/gorm"
)

type trafficCalibrationRequest struct {
	TargetUp     *int64 `json:"target_up"`
	TargetDown   *int64 `json:"target_down"`
	TargetTotal  *int64 `json:"target_total"`
	CountingType string `json:"counting_type"`
	Cycle        string `json:"cycle"`
	Note         string `json:"note"`
}

func findTrafficCalibrationClient(uuid string) (models.Client, error) {
	var client models.Client
	err := dbcore.GetDBInstance().
		Select("uuid", "name", "traffic_limit_type", "traffic_reset_day", "traffic_reset_time", "traffic_reset_timezone").
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
	if _, _, err := trafficledger.CurrentTrafficCycleFor(client, time.Now().UTC()); err != nil {
		api.RespondSuccess(c, gin.H{
			"available": false,
			"client":    client.UUID,
			"reason":    "请先在服务器编辑中设置流量重置日",
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
	api.RespondSuccess(c, gin.H{"available": true, "snapshot": snapshot, "counting_type": trafficledger.NormalizedCountingType(client.TrafficLimitType)})
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
	_, cycle, cycleErr := trafficledger.CurrentTrafficCycleFor(client, now)
	if cycleErr != nil || (request.Cycle != "" && request.Cycle != cycle) {
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
	if request.TargetTotal != nil {
		mode = "total:" + countingType
		snapshot, err = trafficledger.CalibrateBillableCycle(c.Request.Context(), dbcore.GetDBInstance(), client, *request.TargetTotal, fmt.Sprint(actor), now)
	} else {
		snapshot, err = trafficledger.CalibrateCurrentCycle(c.Request.Context(), dbcore.GetDBInstance(), client, trafficledger.Usage{Up: *request.TargetUp, Down: *request.TargetDown}, fmt.Sprint(actor), now)
	}
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, "保存流量校准失败："+err.Error())
		return
	}
	auditlog.Log(
		c.ClientIP(),
		fmt.Sprint(actor),
		fmt.Sprintf("calibrated traffic for client %s cycle=%s mode=%s to up=%d down=%d note=%q", client.UUID, cycle, mode, snapshot.Effective.Up, snapshot.Effective.Down, strings.TrimSpace(request.Note)),
		"warn",
	)
	api.RespondSuccess(c, gin.H{"available": true, "snapshot": snapshot, "counting_type": countingType})
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

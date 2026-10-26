package admin

import (
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/expiry"
	"github.com/komari-monitor/komari/web/api"
	"gorm.io/gorm"
	"net/http"
)

func GetBillingPolicy(c *gin.Context) {
	var client models.Client
	err := dbcore.GetDBInstance().Select("uuid", "expired_at", "expiry_timezone").Where("uuid = ?", c.Param("uuid")).First(&client).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		api.RespondError(c, http.StatusNotFound, "服务器不存在")
		return
	}
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "读取账单设置失败："+err.Error())
		return
	}
	api.RespondSuccess(c, billingPolicy(client))
}

func billingPolicy(client models.Client) gin.H {
	zone, err := expiry.NormalizeTimezone(client.ExpiryTimezone)
	if err != nil {
		zone = expiry.DefaultTimezone
	}
	local := ""
	if expiry.IsFinite(client.ExpiredAt) {
		local = expiry.FormatLocalDateTime(*client.ExpiredAt, zone)
	}
	return gin.H{"expired_at": client.ExpiredAt, "expiry_timezone": zone, "expiry_local_datetime": local}
}

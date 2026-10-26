package admin

import (
	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/trafficreset"
	"github.com/komari-monitor/komari/web/api"
	"net/http"
	"time"
)

// The administrator dialog loads the authoritative policy when opened. This
// avoids inferring settings from the public node summary's effective quota.
func GetTrafficPolicy(c *gin.Context) {
	client, err := findTrafficCalibrationClient(c.Param("uuid"))
	if err != nil {
		api.RespondError(c, http.StatusNotFound, "服务器不存在")
		return
	}
	api.RespondSuccess(c, trafficPolicy(client, time.Now().UTC()))
}
func trafficPolicy(client models.Client, now time.Time) gin.H {
	schedule := trafficreset.FromFields(client.TrafficResetDay, client.TrafficResetTime, client.TrafficResetTimezone)
	return gin.H{"traffic_reset_day": client.TrafficResetDay, "traffic_reset_time": trafficreset.FormatClock(schedule.Hour, schedule.Minute, schedule.Second), "traffic_reset_timezone": schedule.Timezone, "next_reset": schedule.FormatNext(now)}
}

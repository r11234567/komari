package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/trafficledger"
	"github.com/komari-monitor/komari/protocol/v1"
	agent_runtime "github.com/komari-monitor/komari/web/agent"
)

var publicClientConnections = make(chan struct{}, 128)

func GetClients(c *gin.Context) {
	// 升级到ws
	if !IsWebSocketUpgrade(c) {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Require WebSocket upgrade"})
		return
	}
	select {
	case publicClientConnections <- struct{}{}:
		defer func() { <-publicClientConnections }()
	default:
		c.Status(http.StatusServiceUnavailable)
		return
	}
	// Upgrade the HTTP connection to a WebSocket connection
	conn, err := UpgradeSafeConn(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Failed to upgrade to WebSocket." + err.Error()})
		return
	}
	defer conn.Close()
	conn.SetReadLimit(256)

	// PublicViewer invariant: /api/clients always filters Hidden nodes and
	// ignores session cookies.
	loadHiddenSet := func() (map[string]bool, error) {
		hiddenMap := map[string]bool{}
		var hiddenClients []models.Client
		db := dbcore.GetDBInstance()
		if err := db.Select("uuid").Where("hidden = ?", true).Find(&hiddenClients).Error; err != nil {
			return nil, err
		}
		for _, cli := range hiddenClients {
			hiddenMap[cli.UUID] = true
		}
		return hiddenMap, nil
	}

	// 请求
	var window time.Time
	var messages int
	for {
		var resp struct {
			Online []string             `json:"online"` // 已建立连接的客户端uuid列表
			Data   map[string]v1.Report `json:"data"`   // 最后上报的数据
		}

		resp.Online = []string{}
		resp.Data = map[string]v1.Report{}

		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Minute)); err != nil {
			return
		}
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		now := time.Now()
		if now.Sub(window) >= time.Second {
			window, messages = now, 0
		}
		messages++
		if messages > 8 || kind != websocket.TextMessage || len(data) > 256 {
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "invalid or excessive status requests"),
				now.Add(time.Second))
			return
		}
		if err := conn.SetWriteDeadline(now.Add(10 * time.Second)); err != nil {
			return
		}
		message := string(data)

		uuID := ""
		if message != "get" { // 非请求全部内容
			if strings.HasPrefix(message, "get ") {
				uuID = strings.TrimSpace(strings.TrimPrefix(message, "get "))
			} else {
				conn.WriteJSON(gin.H{"status": "error", "error": "Invalid message"})
				continue
			}
		}

		hiddenMap, err := loadHiddenSet()
		if err != nil {
			conn.WriteJSON(gin.H{"status": "error", "error": "Failed to load client visibility"})
			continue
		}

		// 在线客户端uuid列表（WebSocket 与非 WebSocket）
		for _, key := range agent_runtime.GetAllOnlineUUIDs() {
			if hiddenMap[key] {
				continue
			}
			if uuID != "" && key != uuID {
				continue
			}
			resp.Online = append(resp.Online, key)
		}

		//过往节点数据信息
		calibrated, _ := trafficledger.CurrentCalibratedCycleUsages(
			c.Request.Context(), dbcore.GetDBInstance(), time.Now().UTC(),
		)
		for key, report := range agent_runtime.GetLatestReport() {
			if hiddenMap[key] {
				continue
			}
			if uuID != "" && key != uuID {
				continue
			}

			report.UUID = "" // 不暴露 uuid
			if report.CPU.Usage == 0 {
				report.CPU.Usage = 0.01
			}
			if usage, ok := calibrated[key]; ok {
				report.Network.TotalUp = usage.Up
				report.Network.TotalDown = usage.Down
			}
			resp.Data[key] = *report
		}

		err = conn.WriteJSON(gin.H{"status": "success", "data": resp})
		if err != nil {
			return
		}
	}
}

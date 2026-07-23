package auditlog

import (
	"context"
	"log"
	"time"

	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
)

func Log(ip, uuid, message, msgType string) {
	now := time.Now()
	logEntry := &models.Log{
		IP:      ip,
		UUID:    uuid,
		Message: message,
		MsgType: msgType,
		Time:    models.FromTime(now),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := dbcore.Write(ctx, func(db *gorm.DB) error {
		return db.Create(logEntry).Error
	}); err != nil {
		log.Printf("Failed to persist audit log: %v", err)
	}
}

func EventLog(eventType, message string) {
	Log("", "", message, eventType)
}

// Delete logs older than 30 days
func RemoveOldLogs() {
	threshold := time.Now().AddDate(0, 0, -30)
	_, err := dbcore.TryMaintenance(context.Background(), func(db *gorm.DB) error {
		if flags.IsSQLite() {
			return db.Exec("DELETE FROM logs WHERE rowid IN (SELECT rowid FROM logs WHERE time < ? LIMIT 5000)", threshold).Error
		}
		return db.Where("time < ?", threshold).Delete(&models.Log{}).Error
	})
	if err != nil {
		log.Println("Failed to remove old logs:", err)
	}
}

package accounts

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	messageevent "github.com/komari-monitor/komari/database/models/messageEvent"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/utils"
	"github.com/komari-monitor/komari/utils/geoip"
	"github.com/komari-monitor/komari/utils/messageSender"
	"gorm.io/gorm"
)

// GetAllSessions 获取所有会话
func GetAllSessions() (sessions []models.Session, err error) {
	db := dbcore.GetDBInstance()
	err = db.Find(&sessions).Error
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

// CreateSession 创建新会话
func CreateSession(uuid string, expires int, userAgent, ip, login_method string) (string, error) {
	db := dbcore.GetDBInstance()
	session := utils.GenerateRandomString(32)

	sessionRecord := models.Session{
		UUID:         uuid,
		Session:      hashSessionToken(session),
		Expires:      time.Now().UTC().Add(time.Duration(expires) * time.Second),
		UserAgent:    userAgent,
		Ip:           ip,
		LoginMethod:  login_method,
		LatestOnline: time.Now().UTC(),
	}
	go func() {
		LoginNotification, _ := config.GetAs[bool](config.LoginNotificationKey, false)
		if LoginNotification {
			ipAddr := net.ParseIP(ip)
			ipinfo, _ := geoip.GetGeoInfo(ipAddr)
			loc := "unknown"
			if ipinfo != nil && ipinfo.Name != "" {
				loc = ipinfo.Name
			}
			messageSender.SendEvent(models.EventMessage{
				Event:   messageevent.Login,
				Time:    time.Now().UTC(),
				Message: fmt.Sprintf("%s: %s (%s)\n%s", login_method, ip, loc, userAgent),
				Emoji:   "🔑",
			})
		}
	}()

	err := db.Create(&sessionRecord).Error
	if err != nil {
		return "", err
	}
	return session, nil
}

// GetSession 根据会话 ID 获取 UUID
func GetSession(session string) (uuid string, err error) {
	sessionRecord, err := findSessionRecord(session)
	if err != nil {
		return "", err
	}

	if time.Now().UTC().After(sessionRecord.Expires) {
		// 会话已过期，删除它
		_ = DeleteSession(session)
		return "", errors.New("session expired")
	}

	return sessionRecord.UUID, nil
}

func GetUserBySession(session string) (models.User, error) {
	sessionRecord, err := findSessionRecord(session)
	if err != nil {
		return models.User{}, err
	}
	return GetUserByUUID(sessionRecord.UUID)
}

// findSessionRecord looks a cookie up against the stored digest, falling back
// to the legacy plaintext row so sessions issued before hashing keep working
// until they expire. A value that is already a digest is refused outright:
// accepting it would make the stored column itself a usable credential.
func findSessionRecord(session string) (models.Session, error) {
	if session == "" || isSessionDigest(session) {
		return models.Session{}, gorm.ErrRecordNotFound
	}
	db := dbcore.GetDBInstance()
	var record models.Session
	if err := db.Where("session = ?", hashSessionToken(session)).First(&record).Error; err == nil {
		return record, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return models.Session{}, err
	}
	if err := db.Where("session = ?", session).First(&record).Error; err != nil {
		return models.Session{}, err
	}
	return record, nil
}

// StoredSessionKey resolves a caller-supplied value to the stored column value,
// accepting a raw cookie, a legacy plaintext row, or a digest read back from
// the session list. Callers that display or compare session identities need the
// stored form, since that is what the session list carries.
func StoredSessionKey(session string) string {
	if session == "" || isSessionDigest(session) {
		return session
	}
	hashed := hashSessionToken(session)
	db := dbcore.GetDBInstance()
	var count int64
	if err := db.Model(&models.Session{}).Where("session = ?", hashed).Count(&count).Error; err == nil && count > 0 {
		return hashed
	}
	return session
}

// DeleteSession 删除指定会话
func DeleteSession(session string) (err error) {
	db := dbcore.GetDBInstance()
	result := db.Where("session = ?", StoredSessionKey(session)).Delete(&models.Session{})
	if result.Error != nil {
		return result.Error
	}
	return nil
}

func DeleteAllSessions() error {
	db := dbcore.GetDBInstance()
	result := db.Where("1 = 1").Delete(&models.Session{})
	if result.Error != nil {
		return result.Error
	}
	return nil
}

func UpdateLatest(session, useragent, ip string) error {
	db := dbcore.GetDBInstance()
	return db.Model(&models.Session{}).Where("session = ?", StoredSessionKey(session)).Updates(map[string]interface{}{
		"latest_online":     time.Now().UTC(),
		"latest_user_agent": useragent,
		"latest_ip":         ip,
	}).Error
}

func RemoveExpiredSessions() error {
	db := dbcore.GetDBInstance()
	result := db.Where("expires < ?", time.Now().UTC()).Delete(&models.Session{})
	if result.Error != nil {
		return result.Error
	}
	return nil
}

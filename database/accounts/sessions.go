package accounts

import (
	"encoding/hex"
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
		Session:      SessionIDForCookie(session),
		Expires:      time.Now().UTC().Add(time.Duration(expires) * time.Second),
		UserAgent:    userAgent,
		Ip:           ip,
		LoginMethod:  login_method,
		LatestOnline: time.Now().UTC(),
	}
	err := db.Create(&sessionRecord).Error
	if err != nil {
		return "", err
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
				Kind:    messageSender.KindLogin,
				Event:   messageevent.Login,
				Time:    time.Now().UTC(),
				Message: fmt.Sprintf("%s: %s (%s)\n%s", login_method, ip, loc, userAgent),
				Emoji:   "🔑",
			})
		}
	}()

	return session, nil
}

// GetSession 根据会话 ID 获取 UUID
func GetSession(session string) (uuid string, err error) {
	uuid, _, err = GetSessionIdentity(session)
	return uuid, err
}

// GetSessionIdentity returns the authenticated user and when the session was
// created. Callers that perform step-up checks use the creation time to bound
// how long a fresh login can authorize another short-lived capability.
func GetSessionIdentity(session string) (uuid string, createdAt time.Time, err error) {
	sessionRecord, err := findSessionRecord(session)
	if err != nil {
		return "", time.Time{}, err
	}

	return sessionRecord.UUID, sessionRecord.CreatedAt, nil
}

func GetUserBySession(session string) (models.User, error) {
	sessionRecord, err := findSessionRecord(session)
	if err != nil {
		return models.User{}, err
	}
	return GetUserByUUID(sessionRecord.UUID)
}

func findSessionRecord(cookie string) (models.Session, error) {
	id := SessionIDForCookie(cookie)
	if id == "" {
		return models.Session{}, gorm.ErrRecordNotFound
	}
	var record models.Session
	err := dbcore.GetDBInstance().Where("session = ? AND expires > ?", id, time.Now().UTC()).First(&record).Error
	return record, err
}

// DeleteSession revokes the session identified by a browser cookie at logout.
func DeleteSession(cookie string) error {
	id := SessionIDForCookie(cookie)
	if id == "" {
		return nil
	}
	return DeleteSessionByID(id)
}

func DeleteSessionByID(id string) error {
	if len(id) != len(sessionDigestPrefix)+64 || !isSessionDigest(id) {
		return ErrInvalidSessionID
	}
	if _, err := hex.DecodeString(id[len(sessionDigestPrefix):]); err != nil {
		return ErrInvalidSessionID
	}
	return dbcore.GetDBInstance().Where("session = ?", id).Delete(&models.Session{}).Error
}

var ErrInvalidSessionID = errors.New("invalid session identifier")

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
	return db.Model(&models.Session{}).Where("session = ? AND expires > ?", SessionIDForCookie(session), time.Now().UTC()).Updates(map[string]interface{}{
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

package enrollment

// Single-use installation tokens.
//
// A permanent agent token embedded in a shell command is a long-lived secret
// in a potentially insecure place: shell history, chat logs, CI environment
// dumps. An install token is short-lived and single-use, so a leaked command
// stops working after one install or after the deadline, whichever comes first.

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
)

const (
	// InstallTokenLifetime is how long a generated install command stays usable.
	// Long enough that an operator can copy the command and run it on a remote
	// host without rushing; short enough that a leaked command cannot be used
	// indefinitely.
	InstallTokenLifetime = 24 * time.Hour
	installTokenBytes    = 32
)

// IssueInstallToken creates a single-use token for the named agent.
func IssueInstallToken(clientUUID string) (string, time.Time, error) {
	raw := make([]byte, installTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := "ki_" + base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().UTC()
	expiresAt := now.Add(InstallTokenLifetime)
	row := models.InstallToken{
		Token:     token,
		Client:    clientUUID,
		ExpiresAt: expiresAt,
		CreatedAt: now,
	}
	if err := dbcore.GetDBInstance().Create(&row).Error; err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

// RedeemInstallToken validates the token and returns the agent UUID it belongs
// to, marking it used so it cannot be presented again.
//
// The token is valid if it exists, has not been used, and has not expired.
// All three conditions are checked in a single transaction so two concurrent
// first-installs from the same token cannot both succeed.
func RedeemInstallToken(token string) (clientUUID string, err error) {
	if token == "" {
		return "", errors.New("install token is required")
	}
	err = dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		var row models.InstallToken
		if err := tx.Where("token = ?", token).First(&row).Error; err != nil {
			return err
		}
		if row.UsedAt != nil {
			return errors.New("install token has already been used")
		}
		if time.Now().UTC().After(row.ExpiresAt) {
			return errors.New("install token has expired")
		}
		now := time.Now().UTC()
		if err := tx.Model(&row).Update("used_at", now).Error; err != nil {
			return err
		}
		clientUUID = row.Client
		return nil
	})
	return clientUUID, err
}

// IsInstallToken reports whether the given token looks like an install token
// without querying the database. It is used by the auth layer to route the
// token to the right handler before a DB lookup.
func IsInstallToken(token string) bool {
	return len(token) > 3 && token[:3] == "ki_"
}

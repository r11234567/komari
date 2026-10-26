package accounts

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/utils/messageSender/factory"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

var ErrCredentialStateChanged = errors.New("account credential state changed; verify again")
var ErrLastCredential = errors.New("bind another contact or enable TOTP before removing the last OTP contact")

func ListCredentialContacts(ctx context.Context, id string) ([]models.CredentialContact, error) {
	contacts := []models.CredentialContact{}
	err := dbcore.GetDBInstance().WithContext(ctx).Where("user_uuid = ?", id).Order("kind").Find(&contacts).Error
	return contacts, err
}

// VerifyCurrentPassword performs a fresh confirmation without migrating a
// legacy hash mid-challenge (which would invalidate the challenge's binding).
func VerifyCurrentPassword(user models.User, password string) bool {
	if password == "" || len(password) > 1024 {
		return false
	}
	if strings.HasPrefix(user.Passwd, "$2") {
		return bcrypt.CompareHashAndPassword([]byte(user.Passwd), []byte(password)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(legacyPasswordHash(password)), []byte(user.Passwd)) == 1
}

// ChangeVerifiedContact is a Go-only action, called only after consuming a new
// destination's BindContact proof. Conditional security-version rotation makes
// concurrent enrollment/replacement decisions serialize across all SQL stores.
func ChangeVerifiedContact(ctx context.Context, user models.User, contact models.CredentialContact) error {
	address, err := factory.NormalizeCredentialAddress(contact.Kind, contact.Address)
	if err != nil || contact.Transport == "" || len(contact.Transport) > 128 {
		return ErrCredentialStateChanged
	}
	contact.UserUUID = user.UUID
	contact.Address = address
	contact.VerifiedAt = time.Now().UTC()
	contact.User = models.User{}
	return dbcore.GetDBInstance().WithContext(ctx).Session(&gorm.Session{Logger: gormlogger.Discard}).Transaction(func(tx *gorm.DB) error {
		if err := rotateCredentialVersion(tx, user); err != nil {
			return err
		}
		return tx.Omit("User").Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_uuid"}, {Name: "kind"}}, DoUpdates: clause.AssignmentColumns([]string{"address", "transport", "verified_at"})}).Create(&contact).Error
	})
}

func RemoveCredentialContact(ctx context.Context, user models.User, kind string) (bool, error) {
	deleted := false
	err := dbcore.GetDBInstance().WithContext(ctx).Session(&gorm.Session{Logger: gormlogger.Discard}).Transaction(func(tx *gorm.DB) error {
		if err := rotateCredentialVersion(tx, user); err != nil {
			return err
		}
		var contacts []models.CredentialContact
		if err := tx.Where("user_uuid = ?", user.UUID).Find(&contacts).Error; err != nil {
			return err
		}
		found := false
		for _, contact := range contacts {
			if contact.Kind == kind {
				found = true
			}
		}
		if !found {
			return gorm.ErrRecordNotFound
		}
		if len(contacts) == 1 && user.TwoFactor == "" {
			return ErrLastCredential
		}
		result := tx.Where("user_uuid = ? AND kind = ?", user.UUID, kind).Delete(&models.CredentialContact{})
		deleted = result.RowsAffected == 1
		return result.Error
	})
	return deleted, err
}

func rotateCredentialVersion(tx *gorm.DB, user models.User) error {
	result := tx.Model(&models.User{}).Where("uuid = ? AND passwd = ? AND credential_version = ?", user.UUID, user.Passwd, user.CredentialVersion).
		Where("(two_factor = ? OR (two_factor IS NULL AND ? = ''))", user.TwoFactor, user.TwoFactor).
		Updates(map[string]any{"credential_version": uuid.NewString(), "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCredentialStateChanged
	}
	return nil
}

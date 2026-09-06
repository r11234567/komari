package accounts

import (
	"errors"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// totpPeriod matches the default period used by totp.Generate, which is what
// enrolled authenticators are provisioned with.
const totpPeriod = 30 * time.Second

// totpCodeCounter finds the time step a code actually belongs to.
//
// Recording the counter for "now" would not stop replay: validation accepts one
// step of skew either way, so a code minted for step N is still accepted while
// the clock reads N+1, under a counter that has not been spent yet. Pinning the
// code to its own step is what makes it single-use.
//
// Returns false when the code matches no step in the accepted window.
func totpCodeCounter(code, secret string, now time.Time) (int64, bool) {
	period := uint(totpPeriod.Seconds())
	for _, offset := range []int64{0, -1, 1} {
		at := now.UTC().Add(time.Duration(offset) * totpPeriod)
		ok, err := totp.ValidateCustom(code, secret, at, totp.ValidateOpts{
			Period:    period,
			Skew:      0,
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err == nil && ok {
			return at.Unix() / int64(period), true
		}
	}
	return 0, false
}

// consumeTOTPCounter records that a TOTP time step has been used and reports
// whether this was the first use.
//
// A code observed once - shoulder-surfed, phished through a proxy, or recovered
// from a log - is otherwise replayable for as long as it validates.
//
// The insert is the check: a unique key on (uuid, counter) means only one
// caller can win, so two requests racing with the same code cannot both pass.
func consumeTOTPCounter(uuid string, counter int64, at time.Time) (bool, error) {
	db := dbcore.GetDBInstance()
	record := models.TwoFactorCounter{
		UUID:      uuid,
		Counter:   counter,
		UsedAt:    at.UTC(),
		ExpiresAt: at.UTC().Add(totpReplayRetention),
	}
	result := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&record)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// totpReplayRetention bounds how long a spent counter is remembered. It only
// has to outlive the validation window - a code cannot be accepted after that
// regardless - plus room for clock skew.
const totpReplayRetention = 10 * time.Minute

// RemoveExpiredTOTPCounters drops counters that can no longer be replayed.
func RemoveExpiredTOTPCounters() error {
	db := dbcore.GetDBInstance()
	return db.Where("expires_at < ?", time.Now().UTC()).Delete(&models.TwoFactorCounter{}).Error
}

// clearTOTPCounters removes a user's spent counters, used when 2FA is disabled
// or re-enrolled so a fresh secret starts with a clean slate.
func clearTOTPCounters(uuid string) error {
	db := dbcore.GetDBInstance()
	err := db.Where("uuid = ?", uuid).Delete(&models.TwoFactorCounter{}).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return nil
}

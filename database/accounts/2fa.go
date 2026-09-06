package accounts

import (
	"image"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/pquerna/otp/totp"
)

var (
	TwoFactorIssuer = "Komari Monitor"
)

func Generate2Fa() (string, image.Image, error) {
	otp, err := totp.Generate(totp.GenerateOpts{
		Issuer:      TwoFactorIssuer,
		AccountName: "komari",
	})
	if err != nil {
		return "", nil, err
	}
	img, err := otp.Image(250, 250)
	if err != nil {
		return "", nil, err
	}
	return otp.Secret(), img, nil
}

func Enable2Fa(uuid, secret string) error {
	db := dbcore.GetDBInstance()
	if err := db.Model(&models.User{}).Where("uuid = ?", uuid).Update("two_factor", secret).Error; err != nil {
		return err
	}
	// Counters are bound to the previous secret; a fresh enrolment starts clean.
	return clearTOTPCounters(uuid)
}

// Verify2Fa checks a TOTP code and consumes it, so the same code cannot be
// presented twice. totp.Validate alone reports only whether a code is currently
// valid, which leaves it valid for the remainder of its window - long enough
// for a code that was observed once to be replayed.
func Verify2Fa(uuid, code string) (bool, error) {
	db := dbcore.GetDBInstance()
	var user models.User
	err := db.Where("uuid = ?", uuid).First(&user).Error
	if err != nil {
		return false, err
	}

	if user.TwoFactor == "" {
		return false, nil // 用户未启用2FA
	}

	now := time.Now()
	counter, valid := totpCodeCounter(code, user.TwoFactor, now)
	if !valid {
		return false, nil
	}

	// Claim the step this code belongs to. A losing racer sees it as already
	// spent, which is the intended outcome: exactly one use succeeds.
	fresh, err := consumeTOTPCounter(uuid, counter, now)
	if err != nil {
		return false, err
	}
	if !fresh {
		return false, nil
	}

	return true, nil
}

func Disable2Fa(uuid string) error {
	db := dbcore.GetDBInstance()
	if err := db.Model(&models.User{}).Where("uuid = ?", uuid).Update("two_factor", "").Error; err != nil {
		return err
	}
	return clearTOTPCounters(uuid)
}

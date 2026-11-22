package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/web/security"
	"github.com/pquerna/otp/totp"
)

func TestEnable2FAValidatesCodeFirstAndChecksRegisteredFactors(t *testing.T) {
	db := dbcore.GetDBInstance()
	if db == nil {
		t.Skip("database unavailable")
	}

	user, err := accounts.CreateAccount("2fa-user-"+uuid.NewString()[:8], "Strong-Password-123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = accounts.DeleteAccountByUsername(user.Username)
		db.Where("user_uuid = ?", user.UUID).Delete(&models.UserPasskey{})
	})

	// Enroll a passkey without TOTP so TwoFactor == "" while HasSecondFactor == true.
	if err := db.Omit("User").Create(&models.UserPasskey{
		CredentialHash: uuid.NewString(),
		CredentialID:   "cred-" + uuid.NewString()[:8],
		UserUUID:       user.UUID,
		RPID:           "panel.example.test",
		Name:           "Primary Key",
		PublicKey:      "pk",
	}).Error; err != nil {
		t.Fatal(err)
	}

	newKey, err := totp.Generate(totp.GenerateOpts{Issuer: "Komari", AccountName: user.Username})
	if err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/admin/2fa/enable", func(c *gin.Context) {
		c.Set("uuid", user.UUID)
		Enable2FA(c)
	})

	// An invalid TOTP code must fail 400 BEFORE checking/consuming step-up.
	badReq := httptest.NewRequest(http.MethodPost, "/api/admin/2fa/enable?code=000000", nil)
	badReq.AddCookie(&http.Cookie{Name: security.TwoFactorSecretCookieName, Value: newKey.Secret()})
	badRec := httptest.NewRecorder()
	r.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("invalid TOTP code status = %d, want 400", badRec.Code)
	}

	// Valid TOTP code for a user who already has a Passkey must require step-up (401).
	validCode, err := totp.GenerateCode(newKey.Secret(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	noStepUpReq := httptest.NewRequest(http.MethodPost, "/api/admin/2fa/enable?code="+validCode, nil)
	noStepUpReq.AddCookie(&http.Cookie{Name: security.TwoFactorSecretCookieName, Value: newKey.Secret()})
	noStepUpRec := httptest.NewRecorder()
	r.ServeHTTP(noStepUpRec, noStepUpReq)
	if noStepUpRec.Code != http.StatusUnauthorized {
		t.Fatalf("passkey user enabling TOTP without step-up = %d, want 401", noStepUpRec.Code)
	}
}

package cmd

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestImportPasskeysCLI_DryRunAndApplyOffline(t *testing.T) {
	prevDBFile := flags.DatabaseFile
	t.Cleanup(func() {
		flags.DatabaseFile = prevDBFile
		RootCmd.SetArgs(nil)
		RootCmd.SetOut(nil)
		RootCmd.SetErr(nil)
	})
	t.Setenv("KOMARI_PASSKEY_RP_ID", "cli.example.test")
	t.Setenv("KOMARI_PASSKEY_ORIGIN", "https://cli.example.test")
	t.Setenv("KOMARI_PASSKEY_ALLOW_SYNCED", "false")

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "komari.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.UserPasskey{}, &models.PasskeyCounter{}, &models.PasskeyImport{}, &models.PasskeyRevocation{}); err != nil {
		t.Fatal(err)
	}
	userID := uuid.NewString()
	if err := db.Create(&models.User{UUID: userID, Username: "cli-admin", Passwd: "unused"}).Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubCOSE, err := cbor.Marshal(map[int]any{
		1:  2,
		3:  -7,
		-1: 1,
		-2: priv.X.FillBytes(make([]byte, 32)),
		-3: priv.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	exportPath := filepath.Join(tmp, "credentials-v1.json")
	payload, _ := json.Marshal(map[string]any{
		"version": 1,
		"credentials": []map[string]any{{
			"userUUID":   userID,
			"id":         enc([]byte("cli-credential-id-1")),
			"publicKey":  enc(pubCOSE),
			"counter":    4,
			"deviceType": "singleDevice",
			"backedUp":   false,
		}},
	})
	if err := os.WriteFile(exportPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	runCLI := func(args ...string) (string, error) {
		var out bytes.Buffer
		RootCmd.SetOut(&out)
		RootCmd.SetErr(&out)
		RootCmd.SetArgs(args)
		err := RootCmd.Execute()
		return out.String(), err
	}

	// Dry-run opens DB read-only and must not insert rows.
	out, err := runCLI("--database", dbPath, "import-passkeys", exportPath)
	if err != nil {
		t.Fatalf("dry-run failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Eligible: 1, conflicts: 0, re-enroll: 0, previously applied: false") {
		t.Fatalf("unexpected dry-run output: %q", out)
	}

	// Apply offline commits records + import marker.
	out, err = runCLI("--database", dbPath, "import-passkeys", "--apply-offline", exportPath)
	if err != nil {
		t.Fatalf("apply-offline failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Eligible: 1, conflicts: 0, re-enroll: 0, previously applied: false") {
		t.Fatalf("unexpected apply output: %q", out)
	}

	// Identical rerun reports previously applied = true.
	out, err = runCLI("--database", dbPath, "import-passkeys", "--apply-offline", exportPath)
	if err != nil {
		t.Fatalf("idempotent rerun failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "previously applied: true") {
		t.Fatalf("expected previously applied=true, got: %q", out)
	}

	// Non-regular file (directory) must be rejected.
	if _, err := runCLI("--database", dbPath, "import-passkeys", tmp); err == nil {
		t.Fatal("expected directory argument to fail")
	}
}

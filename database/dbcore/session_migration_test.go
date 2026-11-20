package dbcore

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRetireLegacySessionRowsIsIdempotentAndPreservesValidDigests(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	if err := db.AutoMigrate(&models.Session{}); err != nil {
		t.Fatal(err)
	}

	validDigest := "v1:" + strings.Repeat("a1", 32)
	rows := []models.Session{
		{UUID: "u1", Session: validDigest, Expires: time.Now().UTC().Add(time.Hour)},
		{UUID: "u1", Session: "plaintext-legacy-token-1234567890", Expires: time.Now().UTC().Add(time.Hour)},
		{UUID: "u1", Session: "v1:" + strings.Repeat("A1", 32), Expires: time.Now().UTC().Add(time.Hour)},
		{UUID: "u1", Session: "v1:short", Expires: time.Now().UTC().Add(time.Hour)},
		{UUID: "u1", Session: "v2:" + strings.Repeat("b2", 32), Expires: time.Now().UTC().Add(time.Hour)},
		{UUID: "u1", Session: "v1:" + strings.Repeat("g1", 32), Expires: time.Now().UTC().Add(time.Hour)},
	}
	for _, r := range rows {
		if err := db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Run migration twice to prove idempotency.
	for pass := 1; pass <= 2; pass++ {
		if err := retireLegacySessionRows(db); err != nil {
			t.Fatalf("pass %d failed: %v", pass, err)
		}
		var remaining []models.Session
		if err := db.Find(&remaining).Error; err != nil {
			t.Fatal(err)
		}
		if len(remaining) != 1 || remaining[0].Session != validDigest {
			t.Fatalf("pass %d remaining = %+v, want only %q", pass, remaining, validDigest)
		}
	}
}

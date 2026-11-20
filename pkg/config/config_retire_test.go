package config

import (
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSetDbRetiresLegacyAdminKeyAndExcludesFromReadsAndWrites(t *testing.T) {
	prevDB := db
	t.Cleanup(func() { db = prevDB })

	dbPath := filepath.Join(t.TempDir(), "config-retire.db")
	gdb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	if err := gdb.AutoMigrate(&ConfigItem{}); err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&ConfigItem{Key: RetiredAdminKey, Value: `"legacy-secret-value-123456"`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&ConfigItem{Key: SitenameKey, Value: `"Komari"`}).Error; err != nil {
		t.Fatal(err)
	}

	SetDb(gdb)

	var count int64
	if err := gdb.Model(&ConfigItem{}).Where("key = ?", RetiredAdminKey).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("SetDb did not delete retired api_key row: count=%d err=%v", count, err)
	}

	if err := Set(RetiredAdminKey, "attempted-write"); err == nil {
		t.Fatal("Set allowed writing retired api_key")
	}
	if err := SetMany(map[string]any{RetiredAdminKey: "attempted-write"}); err == nil {
		t.Fatal("SetMany allowed writing retired api_key")
	}

	many, err := GetMany(map[string]any{RetiredAdminKey: "fallback-default", SitenameKey: "Default"})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := many[RetiredAdminKey]; exists {
		t.Fatalf("GetMany returned retired api_key: %+v", many)
	}
	if err := gdb.Model(&ConfigItem{}).Where("key = ?", RetiredAdminKey).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("GetMany inserted default retired api_key row: count=%d err=%v", count, err)
	}

	all, err := GetAll()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := all[RetiredAdminKey]; exists {
		t.Fatalf("GetAll exposed retired api_key: %+v", all)
	}
}

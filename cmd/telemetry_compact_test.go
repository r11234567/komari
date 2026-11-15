package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestCompactMetadataPreservesSnapshotAndActiveKey(t *testing.T) {
	for _, mode := range []string{"file", "environment", "absent"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			db, err := gorm.Open(sqlite.Open(filepath.Join(dir, "source.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if err = db.Exec("CREATE TABLE calibration (bytes INTEGER); INSERT INTO calibration VALUES (250000000000)").Error; err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(dir, "backup")
			if err = os.Mkdir(backup, 0700); err != nil {
				t.Fatal(err)
			}
			key := filepath.Join(dir, "secret.key")
			env := ""
			want := "file-key-0123456789"
			if mode != "absent" {
				if err = os.WriteFile(key, []byte(want), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "environment" {
				env = "  environment-key-0123456789\n"
				want = "environment-key-0123456789"
			}
			if err = backupCompactMetadata(context.Background(), db, backup, key, env); err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				Bytes     int64  `json:"metadata_bytes"`
				SHA256    string `json:"metadata_sha256"`
				KeySource string `json:"key_source"`
			}
			b, err := os.ReadFile(filepath.Join(backup, "metadata.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(b, &manifest); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(backup, "komari.db"))
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(content)
			if manifest.KeySource != mode || manifest.Bytes != int64(len(content)) || manifest.SHA256 != hex.EncodeToString(hash[:]) {
				t.Fatal("rollback manifest mismatch")
			}
			if mode == "absent" {
				if _, err = os.Stat(filepath.Join(backup, "secret.key")); !os.IsNotExist(err) {
					t.Fatal("absent key must not be fabricated")
				}
			} else {
				b, err = os.ReadFile(filepath.Join(backup, "secret.key"))
				if err != nil || string(b) != want {
					t.Fatal("active decryption key was not preserved")
				}
				info, err := os.Stat(filepath.Join(backup, "secret.key"))
				if err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
					t.Fatal("rollback key must be private")
				}
			}
			restored, err := gorm.Open(sqlite.Open(filepath.Join(backup, "komari.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			restoredRaw, err := restored.DB()
			if err != nil {
				t.Fatal(err)
			}
			defer restoredRaw.Close()
			var amount int64
			if err = restored.Raw("SELECT bytes FROM calibration").Scan(&amount).Error; err != nil || amount != 250000000000 {
				t.Fatal("metadata snapshot changed")
			}
			if err = backupCompactMetadata(context.Background(), db, backup, key, env); err == nil {
				t.Fatal("must not overwrite rollback files")
			}
		})
	}
}

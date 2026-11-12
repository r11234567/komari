package cmd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/historyexport"
	"github.com/spf13/cobra"
	"gorm.io/gorm"
)

func init() {
	var offline, retire bool
	var destination, backup string
	command := &cobra.Command{Use: "telemetry-compact", Short: "Verify rollback material and build a physically compacted replacement SQLite file", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if !offline || destination == "" || backup == "" {
			return fmt.Errorf("stop panel AND export workers, then pass --confirm-offline --output NEW-METRICS.db --backup-dir NEW-BACKUP-DIRECTORY")
		}
		// Normal startup consumes this file and replaces the data directory.
		// Maintenance must not silently turn into a pending restore operation.
		if _, err := os.Lstat(filepath.Join("data", "backup.zip")); err == nil {
			return fmt.Errorf("staged data/backup.zip restore exists; complete or remove that staged restore before offline compaction")
		} else if !os.IsNotExist(err) {
			return err
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := dbcore.Initialize(); err != nil {
			return err
		}
		defer dbcore.Close()
		if err := metricstore.InitializeStore(); err != nil {
			return err
		}
		defer metricstore.CloseStoreContext(context.Background())
		if err := metricstore.ActivateTelemetry(ctx); err != nil {
			return err
		}
		// A running worker must not change queue files while maintenance changes
		// the source generation. Epoch fencing invalidates pre-swap jobs later.
		queueDir := filepath.Join("data", "exports-v2")
		if _, err := os.Stat(queueDir); err == nil {
			release, err := historyexport.LockMaintenance(ctx, queueDir)
			if err != nil {
				return err
			}
			defer release()
		} else if !os.IsNotExist(err) {
			return err
		}
		metadata := dbcore.GetDBInstance()
		if metadata.Dialector.Name() != "sqlite" {
			return fmt.Errorf("offline portable compaction currently requires SQLite metadata as well as SQLite metrics")
		}
		result, err := metricstore.GetStore().Telemetry().CompactSQLite(ctx, destination, backup, retire, func() error {
			return backupCompactMetadata(ctx, metadata, backup, filepath.Join("data", "secret.key"), os.Getenv("KOMARI_SECRET_KEY"))
		})
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(result, "", "  ")
		cmd.Println(string(b))
		cmd.Println("Verified replacement ready. Original database files are retained; no source file was replaced. While all processes remain stopped, install the new metrics file; preserve the original data directory and the verified rollback bundle for at least seven days. Pre-swap exports must be recreated.")
		return nil
	}}
	command.Flags().BoolVar(&offline, "confirm-offline", false, "Confirm panel and all export workers are stopped")
	command.Flags().BoolVar(&retire, "retire-legacy", false, "Remove original legacy tables from the replacement after verifying history and ledger migration")
	command.Flags().StringVar(&destination, "output", "", "New metrics SQLite file; must not already exist")
	command.Flags().StringVar(&backup, "backup-dir", "", "New directory for verified SQL/archive rollback material; never auto-deleted")
	RootCmd.AddCommand(command)
}

// Preserve the active decryption material alongside the metadata snapshot.
// Environment keys take precedence over files in secureconfig as well. Never
// print the material; its copy and manifest are private rollback files.
func backupCompactMetadata(ctx context.Context, db *gorm.DB, directory, keyFile, environmentKey string) error {
	path := filepath.Join(directory, "komari.db")
	if err := db.WithContext(ctx).Exec("VACUUM INTO '" + strings.ReplaceAll(filepath.ToSlash(path), "'", "''") + "'").Error; err != nil {
		return err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "mode=ro"}
	checkDB, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return err
	}
	var check string
	err = checkDB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check)
	closeErr := checkDB.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if check != "ok" {
		return fmt.Errorf("metadata rollback integrity: %s", check)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	if err == nil {
		err = f.Sync()
	}
	closeErr = f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	manifest := struct {
		Version   int    `json:"version"`
		Bytes     int64  `json:"metadata_bytes"`
		SHA256    string `json:"metadata_sha256"`
		KeySource string `json:"key_source"`
	}{Version: 1, Bytes: n, SHA256: hex.EncodeToString(hash.Sum(nil)), KeySource: "absent"}
	var key io.Reader
	if active := strings.TrimSpace(environmentKey); active != "" {
		key = strings.NewReader(active)
		manifest.KeySource = "environment"
	} else {
		source, err := os.Open(keyFile)
		if err == nil {
			defer source.Close()
			info, err := source.Stat()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("decryption key must be a regular file")
			}
			key = source
			manifest.KeySource = "file"
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if key != nil {
		if err := writeCompactRollbackFile(filepath.Join(directory, "secret.key"), key); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeCompactRollbackFile(filepath.Join(directory, "metadata.json"), strings.NewReader(string(b)+"\n"))
}

func writeCompactRollbackFile(path string, source io.Reader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, source)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	appconfig "github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/migrations"
	"github.com/spf13/cobra"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

var (
	legacyImportSource          string
	legacyImportIncludeLongTerm bool
	legacyImportConfirmOffline  bool
)

var ImportLegacyHistoryCmd = &cobra.Command{
	Use:   "import-legacy-history",
	Short: "Import raw monitoring history from a legacy Komari SQLite database",
	RunE: func(cmd *cobra.Command, args []string) error {
		if !legacyImportConfirmOffline {
			return fmt.Errorf("refusing to write metrics while Komari may be running; pass --confirm-offline after stopping the service")
		}
		sourcePath, err := filepath.Abs(strings.TrimSpace(legacyImportSource))
		if err != nil || strings.TrimSpace(legacyImportSource) == "" {
			return fmt.Errorf("resolve source database: %w", err)
		}
		info, err := os.Stat(sourcePath)
		if err != nil {
			return fmt.Errorf("inspect source database: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("source database is not a regular file")
		}
		targetPath, err := filepath.Abs(flagsDatabaseFile())
		if err != nil {
			return fmt.Errorf("resolve target database: %w", err)
		}
		if samePath(sourcePath, targetPath) {
			return fmt.Errorf("source and active Komari databases must be different files")
		}

		source, closeSource, err := openLegacyImportSource(sourcePath)
		if err != nil {
			return err
		}
		defer closeSource()
		if err := dbcore.Initialize(); err != nil {
			return fmt.Errorf("open active Komari database: %w", err)
		}
		defer dbcore.Close()
		activeDB := dbcore.GetDBInstance()
		if err := validateLegacyImportIdentities(source, activeDB); err != nil {
			return err
		}

		cfg, err := appconfig.GetManyAs[metricstore.MetricStoreConfig]()
		if err != nil {
			return fmt.Errorf("read active metric store configuration: %w", err)
		}
		if cfg.DownsamplingEnabled {
			return fmt.Errorf("raw history import requires metric downsampling to be disabled")
		}
		retentionDays, err := legacyImportRetentionDays(source, legacyImportIncludeLongTerm)
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		store, err := metricstore.OpenStoreForMigration(ctx, cfg, retentionDays)
		if err != nil {
			return fmt.Errorf("open active metric store: %w", err)
		}
		defer store.Close()

		cmd.Printf("Importing legacy history from %s\n", sourcePath)
		cmd.Printf("Target metric store: %s (downsampling disabled)\n", metricstore.ResolveDriverFromConfig(cfg.Driver, cfg.DSN))
		cmd.Printf("records_long_term: %s\n", map[bool]string{true: "included by explicit request", false: "excluded (raw-only mode)"}[legacyImportIncludeLongTerm])
		lastLog := time.Time{}
		stats, err := migrations.MigrateLegacyMonitoringWithOptions(ctx, source, store, migrations.LegacyMonitoringImportOptions{
			IncludeLongTerm: legacyImportIncludeLongTerm,
		}, func(progress migrations.LegacyMonitoringProgress) {
			now := time.Now()
			if !lastLog.IsZero() && now.Sub(lastLog) < 5*time.Second && progress.SourceRowsDone < progress.SourceRowsTotal {
				return
			}
			lastLog = now
			percent := float64(0)
			if progress.SourceRowsTotal > 0 {
				percent = float64(progress.SourceRowsDone) * 100 / float64(progress.SourceRowsTotal)
			}
			cmd.Printf("[%s] table=%s rows=%d/%d (%.2f%%) points=%d\n",
				now.UTC().Format(time.RFC3339), progress.Table, progress.SourceRowsDone,
				progress.SourceRowsTotal, percent, progress.WrittenPoints)
		})
		if err != nil {
			return fmt.Errorf("import legacy history: %w", err)
		}
		cmd.Printf("Import completed: records=%d gpu=%d ping=%d\n", stats.Records, stats.GPU, stats.Ping)
		return nil
	},
}

func init() {
	ImportLegacyHistoryCmd.Flags().StringVar(&legacyImportSource, "source", "", "path to the legacy komari.db")
	ImportLegacyHistoryCmd.Flags().BoolVar(&legacyImportIncludeLongTerm, "include-long-term", false, "also import aggregated records_long_term rows")
	ImportLegacyHistoryCmd.Flags().BoolVar(&legacyImportConfirmOffline, "confirm-offline", false, "confirm that the Komari service is stopped")
	_ = ImportLegacyHistoryCmd.MarkFlagRequired("source")
	RootCmd.AddCommand(ImportLegacyHistoryCmd)
}

func flagsDatabaseFile() string {
	value, err := RootCmd.PersistentFlags().GetString("database")
	if err == nil && strings.TrimSpace(value) != "" {
		return value
	}
	return "./data/komari.db"
}

func samePath(left, right string) bool {
	leftResolved, leftErr := filepath.EvalSymlinks(left)
	rightResolved, rightErr := filepath.EvalSymlinks(right)
	if leftErr == nil {
		left = leftResolved
	}
	if rightErr == nil {
		right = rightResolved
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func openLegacyImportSource(path string) (*gorm.DB, func(), error) {
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&immutable=1&_query_only=1"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		return nil, func() {}, fmt.Errorf("open legacy source database: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, func() {}, fmt.Errorf("access legacy source database: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	closeFn := func() { _ = sqlDB.Close() }
	var required int
	if err := db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('records', 'gpu_records', 'ping_records')").Scan(&required).Error; err != nil {
		closeFn()
		return nil, func() {}, fmt.Errorf("inspect legacy source schema: %w", err)
	}
	if required == 0 {
		closeFn()
		return nil, func() {}, fmt.Errorf("source database contains no legacy monitoring tables")
	}
	return db, closeFn, nil
}

func validateLegacyImportIdentities(source, active *gorm.DB) error {
	var sourceClients, activeClients []models.Client
	if err := source.Select("uuid").Find(&sourceClients).Error; err != nil {
		return fmt.Errorf("list source clients: %w", err)
	}
	if err := active.Select("uuid").Find(&activeClients).Error; err != nil {
		return fmt.Errorf("list active clients: %w", err)
	}
	activeClientIDs := make(map[string]struct{}, len(activeClients))
	for _, client := range activeClients {
		activeClientIDs[client.UUID] = struct{}{}
	}
	missingClients := make([]string, 0)
	for _, client := range sourceClients {
		if _, ok := activeClientIDs[client.UUID]; !ok {
			missingClients = append(missingClients, client.UUID)
		}
	}
	var sourceTasks, activeTasks []models.PingTask
	if err := source.Select("id").Find(&sourceTasks).Error; err != nil {
		return fmt.Errorf("list source ping tasks: %w", err)
	}
	if err := active.Select("id").Find(&activeTasks).Error; err != nil {
		return fmt.Errorf("list active ping tasks: %w", err)
	}
	activeTaskIDs := make(map[uint]struct{}, len(activeTasks))
	for _, task := range activeTasks {
		activeTaskIDs[task.Id] = struct{}{}
	}
	missingTasks := make([]string, 0)
	for _, task := range sourceTasks {
		if _, ok := activeTaskIDs[task.Id]; !ok {
			missingTasks = append(missingTasks, fmt.Sprint(task.Id))
		}
	}
	if len(missingClients) > 0 || len(missingTasks) > 0 {
		sort.Strings(missingClients)
		sort.Strings(missingTasks)
		return fmt.Errorf("source identities are absent from the active database (clients=%s ping_tasks=%s)", strings.Join(missingClients, ","), strings.Join(missingTasks, ","))
	}
	return nil
}

func legacyImportRetentionDays(source *gorm.DB, includeLongTerm bool) (int, error) {
	tables := []string{"records", "gpu_records", "ping_records"}
	if includeLongTerm {
		tables = append(tables, "records_long_term")
	}
	var oldest time.Time
	for _, table := range tables {
		if !source.Migrator().HasTable(table) {
			continue
		}
		var raw sql.NullString
		if err := source.Table(table).Select("MIN(time)").Scan(&raw).Error; err != nil {
			return 0, fmt.Errorf("read oldest timestamp from %s: %w", table, err)
		}
		if !raw.Valid || strings.TrimSpace(raw.String) == "" {
			continue
		}
		value, err := parseLegacyImportTime(raw.String)
		if err != nil {
			return 0, fmt.Errorf("parse oldest timestamp from %s: %w", table, err)
		}
		if oldest.IsZero() || value.Before(oldest) {
			oldest = value
		}
	}
	if oldest.IsZero() {
		return 1, nil
	}
	days := int(time.Since(oldest).Hours()/24) + 2
	if days < 1 {
		days = 1
	}
	return days, nil
}

func parseLegacyImportTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	formats := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}
	var lastErr error
	for _, format := range formats {
		value, err := time.Parse(format, raw)
		if err == nil {
			return value, nil
		}
		lastErr = err
	}
	return time.Time{}, lastErr
}

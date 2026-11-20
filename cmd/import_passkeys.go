package cmd

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/komari-monitor/komari/internal/passkeycredential"
	"github.com/spf13/cobra"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	var apply bool
	c := &cobra.Command{Use: "import-passkeys FILE", Short: "Check a v1 public credential export against the upgraded primary database", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		f, err := os.Open(args[0])
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("expected a regular credential export file")
		}
		path, err := filepath.Abs(flagsDatabaseFile())
		if err != nil {
			return err
		}
		u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
		if filepath.VolumeName(path) != "" {
			u.Path = "/" + u.Path
		}
		q := u.Query()
		q.Set("mode", "ro")
		if apply {
			q.Set("mode", "rw")
			q.Set("_txlock", "immediate")
		}
		q.Set("_foreign_keys", "on")
		u.RawQuery = q.Encode()
		db, err := gorm.Open(sqlite.Open(u.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			return err
		}
		sqlDB, err := db.DB()
		if err != nil {
			return err
		}
		defer sqlDB.Close()
		report, err := passkeycredential.Import(c.Context(), db, f, apply)
		c.Printf("Eligible: %d, conflicts: %d, re-enroll: %d, previously applied: %t\n", report.Eligible, report.Conflicts, report.Rebind, report.AlreadyApplied)
		return err
	}}
	c.Flags().BoolVar(&apply, "apply-offline", false, "Commit the import after stopping the service and freezing legacy credential writes")
	RootCmd.AddCommand(c)
}

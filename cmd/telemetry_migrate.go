package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/trafficledger"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/spf13/cobra"
)

func init() {
	var offline bool
	command := &cobra.Command{Use: "telemetry-migrate", Short: "Resume an offline legacy history migration; preserve original tables for rollback", RunE: func(cmd *cobra.Command, args []string) error {
		if !offline {
			return fmt.Errorf("stop the panel, take a complete backup, and pass --confirm-offline")
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := dbcore.Initialize(); err != nil {
			return err
		}
		defer dbcore.Close()
		cfg, err := config.GetManyAs[metricstore.MetricStoreConfig]()
		if err != nil {
			return err
		}
		if err = metricstore.InitializeStore(); err != nil {
			return err
		}
		defer metricstore.CloseStoreContext(context.Background())
		// Settle business history while the legacy reader is still available.
		// This is an explicit offline operation, never a page-open side effect.
		if err = trafficledger.MigrateTelemetryLedger(ctx, dbcore.GetDBInstance(), time.Now()); err != nil {
			return err
		}
		var points int64
		last := time.Time{}
		err = metricstore.MigrateTelemetryOffline(ctx, metricstore.GetStore(), cfg, func(name string, n int64) {
			points += n
			if time.Since(last) > 5*time.Second {
				cmd.Printf("Migrating %s: %d raw points this invocation\n", name, points)
				last = time.Now()
			}
		})
		if err != nil {
			return err
		}
		cmd.Println("Migration complete. Original tables are retained. Restart the panel to activate the archive engine; rollback requires the pre-migration complete backup.")
		return nil
	}}
	command.Flags().BoolVar(&offline, "confirm-offline", false, "Confirm the panel is stopped and a complete backup exists")
	RootCmd.AddCommand(command)
}

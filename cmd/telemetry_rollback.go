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
	"github.com/spf13/cobra"
)

func init() {
	var offline bool
	var destination string
	command := &cobra.Command{Use: "telemetry-rollback", Short: "Prepare a separate legacy-readable metrics database without losing accepted hot/cold samples", RunE: func(cmd *cobra.Command, args []string) error {
		if !offline || destination == "" {
			return fmt.Errorf("stop the panel, preserve a complete backup, and pass --confirm-offline --output NEW-METRICS.db")
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
		trafficledger.ConfigureTelemetry(dbcore.GetDBInstance())
		// Finish accepted accounting before changing the metric read format.
		for {
			n, err := metricstore.DeliverTelemetryTraffic(ctx, 256, trafficledger.ConsumeTelemetry)
			if err != nil {
				return err
			}
			if n < 256 {
				break
			}
		}
		last := time.Time{}
		err := metricstore.GetStore().Telemetry().RollbackLegacy(ctx, destination, func(n int64) {
			if time.Since(last) > 5*time.Second {
				cmd.Printf("Replayed %d retained raw points\n", n)
				last = time.Now()
			}
		})
		if err != nil {
			return err
		}
		cmd.Println("Rollback SQL prepared. Preserve the current metadata database and calibration ledger; stop all processes before swapping the prepared metrics file. Original SQL and archives remain unchanged.")
		return nil
	}}
	command.Flags().BoolVar(&offline, "confirm-offline", false, "Confirm the panel is stopped and a full backup exists")
	command.Flags().StringVar(&destination, "output", "", "New metrics SQLite path; must not already exist")
	RootCmd.AddCommand(command)
}

package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/historyexport"
	logger "github.com/komari-monitor/komari/utils/log"
)

func publishExportPolicy(ctx context.Context) error {
	p := historyexport.Policy{Nodes: map[string]bool{}, Owners: map[string]bool{}}
	var nodes []models.Client
	var owners []models.User
	db := dbcore.GetDBInstance().WithContext(ctx)
	if e := db.Select("uuid").Limit(8193).Find(&nodes).Error; e != nil {
		return e
	}
	if e := db.Select("uuid").Limit(8193).Find(&owners).Error; e != nil {
		return e
	}
	if len(nodes) > 8192 || len(owners) > 8192 {
		return errors.New("background export authorization snapshot exceeds budget")
	}
	for _, node := range nodes {
		p.Nodes[node.UUID] = true
	}
	for _, owner := range owners {
		p.Owners[owner.UUID] = true
	}
	store, release, e := metricstore.AcquireStore(ctx)
	if e != nil {
		return e
	}
	defer release()
	if store.Telemetry() == nil {
		return errors.New("telemetry not activated")
	}
	p.State, e = store.Telemetry().ExportState(ctx)
	if e != nil {
		return e
	}
	queue := metricstore.ReportQueueDiagnostics()
	switch {
	case historyexport.ForegroundPressure():
		p.Pressure = "foreground_latency"
	case queue.Capacity > 0 && queue.Depth > queue.Capacity/4:
		p.Pressure = "ingestion_backlog"
	case p.State.Pending > 1024:
		p.Pressure = "telemetry_backlog"
	}
	return historyexport.WritePolicy("./data/exports-v2", p)
}
func (a *App) startExportPolicy() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		lastError := ""
		for {
			work, stop := context.WithTimeout(ctx, 2*time.Second)
			err := publishExportPolicy(work)
			stop()
			if err != nil && ctx.Err() == nil {
				if err.Error() != lastError {
					logger.Warnf("export-policy", "Background exports pause until instance state is available: %v", err)
					lastError = err.Error()
				}
			} else if err == nil && lastError != "" {
				logger.Infof("export-policy", "Background export instance state is available again")
				lastError = ""
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	a.addCleanup("export-policy", func(shutdown context.Context) error {
		cancel()
		select {
		case <-done:
			return nil
		case <-shutdown.Done():
			return shutdown.Err()
		}
	})
}

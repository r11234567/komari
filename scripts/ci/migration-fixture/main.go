package main

// Synthetic legacy history for the disposable upgrade fixture. Credentials
// come only from the real HTTPS installation backup; they are never uploaded.
import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/metric"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	root := os.Getenv("E2E_ROOT")
	if exec.Command("bash", "scripts/ci/e2e/guard.sh").Run() != nil || root != filepath.Join(os.Getenv("RUNNER_TEMP"), "komari-e2e") {
		return fmt.Errorf("disposable hosted fixture only")
	}
	dir := filepath.Join(root, "migration-panel", "data")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	bundle, err := zip.OpenReader(filepath.Join(root, "real-full-backup.zip"))
	if err != nil {
		return err
	}
	defer bundle.Close()
	for _, f := range bundle.File {
		if f.Name != "komari.db" && f.Name != "secret.key" {
			continue
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(filepath.Join(dir, f.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			in.Close()
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		ce := out.Close()
		if err != nil {
			return err
		}
		if ce != nil {
			return ce
		}
	}
	var scope struct {
		AgentID string `json:"agentId"`
	}
	data, err := os.ReadFile(filepath.Join(root, "qualification-scope.json"))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &scope); err != nil {
		return err
	}
	if scope.AgentID == "" {
		return fmt.Errorf("missing actually enrolled Agent")
	}
	ctx := context.Background()
	s, err := metric.Open(ctx, metric.SQLite(filepath.Join(dir, "metrics.db"), metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
	if err != nil {
		return err
	}
	defer s.Close()
	if err = metricstore.EnsureBuiltinMetricDefinitions(ctx, s); err != nil {
		return err
	}
	cutoff := time.Now().UTC().Add(-time.Minute).Truncate(time.Minute)
	start := cutoff.Add(-24 * time.Hour)
	var batch []metric.Point
	for i := 0; i < 8640; i++ {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		for name, value := range map[string]float64{metricstore.MetricCPU: float64(i % 100), metricstore.MetricNetTotalUp: float64(1000000 + i*100), metricstore.MetricNetTotalDown: float64(2000000 + i*200), metricstore.MetricTrafficUp: 100, metricstore.MetricTrafficDown: 200} {
			batch = append(batch, metric.Point{MetricName: name, EntityID: scope.AgentID, Timestamp: at, Value: value})
		}
		if len(batch) >= 1000 {
			if err = s.WriteBatch(ctx, batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err = s.WriteBatch(ctx, batch); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(root, "migration-manifest.json"), []byte(fmt.Sprintf("{\"synthetic_points\":43200,\"agentId\":%q}", scope.AgentID)), 0600)
}

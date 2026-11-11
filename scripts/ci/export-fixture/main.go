// export-fixture creates explicitly synthetic cold load only in the guarded,
// disposable GitHub runner. It uses the actual ingestion/archive APIs. It is
// never built into the panel, and its points are not claimed as Agent history.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func main() {
	root := os.Getenv("E2E_ROOT")
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || root != filepath.Join(os.Getenv("RUNNER_TEMP"), "komari-e2e") {
		panic("disposable GitHub fixture only")
	}
	b, e := os.ReadFile(filepath.Join(root, "qualification-scope.json"))
	must(e)
	var scope struct {
		AgentID    string `json:"agentId"`
		PingTaskID uint32 `json:"pingTaskId"`
	}
	must(json.Unmarshal(b, &scope))
	if scope.AgentID == "" || scope.PingTaskID == 0 {
		panic("missing enrolled Agent or real TCP probe")
	}
	ctx := context.Background()
	s, e := metric.Open(ctx, metric.SQLite(filepath.Join(root, "panel/data/metrics.db"), metric.WithTablePrefix("metric_"), metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
	must(e)
	defer s.Close()
	tel, e := s.OpenTelemetry(ctx, metric.TelemetryConfig{ArchiveDirectory: filepath.Join(root, "panel/data/telemetry"), RawWindow: time.Minute, SegmentDuration: time.Minute})
	must(e)
	defer tel.Close()
	const count = 150000
	at := time.Now().UTC().Add(-12 * time.Hour).Truncate(time.Minute)
	labels := map[string]string{"origin": "synthetic_cold_csv_qualification", "payload": strings.Repeat("abcd0123", 1024)}
	for base := 0; base < count; base += 128 {
		points := make([]metric.Point, 0, 128)
		for i := base; i < min(base+128, count); i++ {
			points = append(points, metric.Point{MetricName: "cpu.usage", EntityID: scope.AgentID, Timestamp: at.Add(time.Duration(i) * time.Millisecond), Value: float64(i % 100), Tags: map[string]string{"fixture": "cold_csv_qualification"}, Labels: labels})
		}
		_, e = tel.Accept(ctx, metric.TelemetryBatch{Source: scope.AgentID, Identity: fmt.Sprintf("csv-qualification-%d", base), Points: points}, time.Now())
		must(e)
		for {
			n, e := tel.SummarizeNext(ctx)
			must(e)
			if n == 0 {
				break
			}
		}
		if base%4096 == 0 || base+128 >= count {
			_, e = tel.SealDue(ctx, time.Now().Add(2*time.Hour))
			must(e)
			for {
				more, e := tel.ArchiveNext(ctx)
				must(e)
				if !more {
					break
				}
			}
			for {
				n, e := tel.DeleteArchivedChunk(ctx, time.Now().Add(2*time.Hour), 512)
				must(e)
				if n == 0 {
					break
				}
			}
			fmt.Printf("synthetic cold fixture: %d/%d\n", min(base+128, count), count)
		}
	}
	state, e := tel.ExportState(ctx)
	must(e)
	manifest := map[string]any{"origin": "synthetic load; real installed Agent continues separately", "records": count, "payload_bytes_per_record": 8192, "agentId": scope.AgentID, "pingTaskId": scope.PingTaskID, "start": at, "end": at.Add(time.Duration(count)*time.Millisecond + time.Second), "state": state, "io_budget_bytes_per_second": 131072}
	b, e = json.MarshalIndent(manifest, "", "  ")
	must(e)
	must(os.WriteFile(filepath.Join(root, "qualification-manifest.json"), b, 0600))
}

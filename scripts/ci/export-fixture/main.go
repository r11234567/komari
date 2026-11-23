// export-fixture creates explicitly synthetic cold load only in the guarded,
// disposable GitHub runner. It uses the actual ingestion/archive APIs. It is
// never built into the panel, and its points are not claimed as Agent history.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
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
	if exec.Command("bash", "scripts/ci/e2e/guard.sh").Run() != nil || root != filepath.Join(os.Getenv("RUNNER_TEMP"), "komari-e2e") {
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
	// Use seeded, distinct payloads so compression does not eliminate most
	// archive reads. At 128 KiB/s, reading the compressed input and writing
	// the CSV both count toward the two-hour active-work requirement.
	const count = 80000
	const payloadBytes = 8192
	at := time.Now().UTC().Add(-12 * time.Hour).Truncate(time.Minute)
	random := rand.NewChaCha8([32]byte{1})
	payload := make([]byte, payloadBytes/2)
	tags := map[string]string{"fixture": "cold_csv_qualification"}
	drainColdArchive(ctx, tel, time.Now())
	for base := 0; base < count; base += 128 {
		points := make([]metric.Point, 0, 128)
		for i := base; i < min(base+128, count); i++ {
			_, e := random.Read(payload)
			must(e)
			labels := map[string]string{"origin": "synthetic_cold_csv_qualification", "payload": hex.EncodeToString(payload)}
			points = append(points, metric.Point{MetricName: "cpu.usage", EntityID: scope.AgentID, Timestamp: at.Add(time.Duration(i) * time.Millisecond), Value: float64(i % 100), Tags: tags, Labels: labels})
		}
		_, e = tel.Accept(ctx, metric.TelemetryBatch{Source: scope.AgentID, Identity: fmt.Sprintf("csv-qualification-%d", base), Points: points}, time.Now())
		must(e)
		// Drain sealed segments between batches to stay within the engine's
		// real hot-storage budget. Force the final open segment cold as well.
		sealAt := time.Now()
		if base+128 >= count {
			sealAt = sealAt.Add(time.Minute)
		}
		drainColdArchive(ctx, tel, sealAt)
		if base%4096 == 0 || base+128 >= count {
			fmt.Printf("synthetic cold fixture: %d/%d\n", min(base+128, count), count)
		}
	}
	state, e := tel.ExportState(ctx)
	must(e)
	manifest := map[string]any{"origin": "synthetic load; real installed Agent continues separately", "records": count, "payload_bytes_per_record": payloadBytes, "agentId": scope.AgentID, "pingTaskId": scope.PingTaskID, "start": at, "end": at.Add(time.Duration(count)*time.Millisecond + time.Second), "state": state, "io_budget_bytes_per_second": 131072}
	b, e = json.MarshalIndent(manifest, "", "  ")
	must(e)
	must(os.WriteFile(filepath.Join(root, "qualification-manifest.json"), b, 0600))
}

func drainColdArchive(ctx context.Context, tel *metric.Telemetry, sealAt time.Time) {
	for {
		n, err := tel.SummarizeNext(ctx)
		must(err)
		if n == 0 {
			break
		}
	}
	_, err := tel.SealDue(ctx, sealAt)
	must(err)
	for {
		more, err := tel.ArchiveNext(ctx)
		must(err)
		if !more {
			break
		}
	}
	for {
		n, err := tel.DeleteArchivedChunk(ctx, time.Now(), 512)
		must(err)
		if n == 0 {
			return
		}
	}
}

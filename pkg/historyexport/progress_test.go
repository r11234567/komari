package historyexport

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Exercise the actual durable queue and all three CSV paths, rather than a UI
// snapshot that could hide missing task metadata.
func TestWorkerPersistsPhaseObservation(t *testing.T) {
	for _, mode := range []string{"raw", "summary", "sample30s"} {
		t.Run(mode, func(t *testing.T) {
			w, j := workerFixture(t, mode)
			j = finishWorker(t, w, j)
			if err := w.Queue.Close(); err != nil {
				t.Fatal(err)
			}
			q, err := Open(w.Queue.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			got, err := q.Get(context.Background(), j.Owner, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err = json.Unmarshal(b, &body); err != nil {
				t.Fatal(err)
			}
			if body["phase_started_at"] == nil || body["phase_eta"] == nil || body["throughput"] == nil {
				t.Fatalf("durable task missing phase/throughput/ETA observation: %s", b)
			}
		})
	}
}

func TestProgressRateETAAndWaitingAcrossDurableRecovery(t *testing.T) {
	ctx := context.Background()
	w, j := workerFixture(t, "raw")
	now := time.Now().UTC()
	j.Status, j.Phase, j.ProgressKind, j.PhaseTotal = "running", "decoding_and_generating", "input_units", 100
	j.observation.Comparable = true
	j.observe(now)
	started := j.PhaseStartedAt
	for i := 1; i <= 6; i++ {
		j.PhaseCompleted = int64(i)
		j.WrittenRows, j.OutputBytes, j.DecodedRecords = int64(i*100), int64(i*1000), int64(i*110)
		j.observe(now.Add(time.Duration(i) * 10 * time.Second))
	}
	if j.PhaseETA.State != "stable" || j.PhaseETA.Scope != "phase" || j.Throughput.RowsPerSecond != 10 || j.PhaseActiveSeconds != 60 || !j.PhaseStartedAt.Equal(started) {
		t.Fatalf("wrong rate/phase ETA: %+v", j)
	}
	if j.PhaseETA.LowSeconds != 705 || j.PhaseETA.HighSeconds != 1410 {
		t.Fatal(j.PhaseETA)
	}
	// Same on-disk serialization used by Save/Get/backup, with no test-only
	// sampler injected into the API or reconstructed from a percentage.
	b, err := encodeJob(j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Queue.db.ExecContext(ctx, "UPDATE jobs SET body=?,status=? WHERE id=?", b, j.Status, j.ID); err != nil {
		t.Fatal(err)
	}
	if err = w.Queue.Close(); err != nil {
		t.Fatal(err)
	}
	q, err := Open(w.Queue.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	var persisted []byte
	if err = q.db.QueryRowContext(ctx, "SELECT body FROM jobs WHERE id=?", j.ID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	j, err = decodeJob(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if j.PhaseETA.State != "stable" || len(j.observation.Units) != 6 {
		t.Fatal("sampler not durable")
	}
	j.Status = "paused"
	j.observe(now.Add(65 * time.Second))
	if j.PhaseETA.State == "stable" || j.Throughput.RowsPerSecond != 0 || j.WaitingSince.IsZero() {
		t.Fatal("paused task still forecasts work")
	}
	j.observe(now.Add(125 * time.Second))
	if j.WaitSeconds < 60 || j.PhaseActiveSeconds != 65 {
		t.Fatal("pause counted as processing", j.WaitSeconds, j.PhaseActiveSeconds)
	}
	j.Status = "recovering"
	j.recovered(now.Add(time.Hour))
	j.observe(now.Add(time.Hour))
	j.Status = "running"
	j.observe(now.Add(time.Hour + time.Second))
	if j.PhaseActiveSeconds != 65 || j.PhaseETA.State == "stable" || len(j.observation.Units) != 0 {
		t.Fatal("downtime/recovery reused an old estimate")
	}
	j.WrittenRows = 0
	j.OutputBytes = 0
	j.observe(now.Add(time.Hour + 11*time.Second))
	if j.Throughput.RowsPerSecond < 0 || j.PhaseETA.State == "stable" {
		t.Fatal("rollback produced negative throughput or stale ETA")
	}
}

func TestProgressDoesNotForecastVariableUnknownOrStalledWork(t *testing.T) {
	for _, test := range []struct {
		name       string
		comparable bool
		slow       bool
		stalled    bool
	}{
		{"legacy or mixed", false, false, false}, {"uneven processing", true, true, false}, {"stalled unit", true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			j := &Job{Status: "running", Phase: "decoding_and_generating", ProgressKind: "input_units", PhaseTotal: 100}
			j.observation.Comparable = test.comparable
			j.observe(now)
			for i := 1; i <= 6; i++ {
				j.PhaseCompleted = int64(i)
				d := 10 * time.Second
				if test.slow && i == 6 {
					d = 60 * time.Second
				}
				now = now.Add(d)
				j.observe(now)
			}
			if test.stalled {
				j.estimateETA(now.Add(time.Minute))
			}
			if j.PhaseETA.State == "stable" {
				t.Fatal("unsafe forecast", j.PhaseETA)
			}
		})
	}
}

func TestResourceObservationUsesEffectiveCapacityAndCounterReset(t *testing.T) {
	var sampler resourceSampler
	now := time.Now().UTC()
	p := ResourceProof{Cgroup: "worker", EffectiveCPUs: 1, WorkerCPUs: .03}
	u := sampler.sample(p, resourceCounters{usage: 1000, throttled: 10, memory: 32}, now)
	if u.CPUShare != nil {
		t.Fatal("first sample invented CPU usage")
	}
	p.EffectiveCPUs = .5
	u = sampler.sample(p, resourceCounters{usage: 41000, throttled: 2000, memory: 64}, now.Add(time.Second))
	if u.CPUShare == nil || *u.CPUShare != .08 || u.CPUThrottledSeconds != .002 || u.MemoryCurrentBytes != 64 || u.Scope != "worker_group" {
		t.Fatalf("wrong CPU denominator or memory semantics: %+v", u)
	}
	u = sampler.sample(p, resourceCounters{usage: 10}, now.Add(2*time.Second))
	if u.CPUShare != nil {
		t.Fatal("counter reset was presented as measured usage")
	}
	var disk map[string]any
	j := &Job{Resources: u}
	b, err := encodeJob(j)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &disk); err != nil {
		t.Fatal(err)
	}
	if _, ok := disk["Job"].(map[string]any)["resources"]; ok {
		t.Fatal("ephemeral group measurement persisted as task CPU")
	}
}

package admin

import (
	"testing"
	"time"
)

func TestHistoryExportPingBatchesKeepOneProbeRoundTogether(t *testing.T) {
	base := time.Date(2026, 7, 31, 13, 58, 48, 0, time.UTC)
	observations := []exportPingObservation{
		{timestamp: base, taskID: "1", value: 101},
		{timestamp: base, taskID: "1", value: 0, loss: true},
		{timestamp: base.Add(500 * time.Millisecond), taskID: "2", value: 67},
		{timestamp: base.Add(500 * time.Millisecond), taskID: "2", value: 0, loss: true},
		{timestamp: base.Add(time.Second), taskID: "3", value: -1},
		{timestamp: base.Add(time.Second), taskID: "3", value: 1, loss: true},
	}
	batches := historyExportPingBatches(observations)
	if len(batches) != 1 {
		t.Fatalf("batch count = %d, want 1", len(batches))
	}
	if len(batches[0].ping) != 3 {
		t.Fatalf("task count = %d, want 3", len(batches[0].ping))
	}
}

func TestAttachHistoryExportPingBatchUsesNearestResourceRow(t *testing.T) {
	base := time.Date(2026, 7, 31, 13, 58, 0, 0, time.UTC)
	rows := map[int64]*exportRow{}
	rowFor := func(timestamp time.Time) *exportRow {
		timestamp = timestamp.UTC().Truncate(time.Second)
		key := timestamp.UnixNano()
		if rows[key] == nil {
			rows[key] = &exportRow{timestamp: timestamp, values: map[string][]string{}, ping: map[string]*exportPingValue{}}
		}
		return rows[key]
	}
	previous := rowFor(base)
	previous.values["cpu.usage"] = []string{"10.00"}
	next := rowFor(base.Add(time.Minute))
	next.values["cpu.usage"] = []string{"11.00"}

	batch := &exportPingBatch{
		first: base.Add(48 * time.Second),
		last:  base.Add(49 * time.Second),
		ping: map[string]*exportPingValue{
			"1": &exportPingValue{latency: []float64{101}, loss: []float64{0}},
			"2": &exportPingValue{latency: []float64{-1}, loss: []float64{1}},
		},
	}
	attachHistoryExportPingBatches(rows, []*exportPingBatch{batch}, rowFor)
	if len(previous.ping) != 0 || len(next.ping) != 2 {
		t.Fatalf("nearest-row attachment: previous=%d next=%d", len(previous.ping), len(next.ping))
	}
}

func TestHasGPUDeviceTreatsAgentNoneAsAbsent(t *testing.T) {
	for _, value := range []string{"", "None", "unknown", "N/A"} {
		if hasGPUDevice(value) {
			t.Fatalf("hasGPUDevice(%q) = true", value)
		}
	}
	if !hasGPUDevice("NVIDIA GeForce RTX 4060 Laptop GPU") {
		t.Fatal("real GPU device was treated as absent")
	}
}

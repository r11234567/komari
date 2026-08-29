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

func TestAttachHistoryExportObservationsKeepsOneSamplePerSlot(t *testing.T) {
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rows := map[int64]*exportRow{}
	rowFor := func(timestamp time.Time) *exportRow {
		timestamp = timestamp.UTC().Truncate(historyExportSampleStep)
		key := timestamp.UnixNano()
		if rows[key] == nil {
			rows[key] = &exportRow{
				timestamp: timestamp,
				values:    map[string][]string{},
				ping:      map[string]*exportPingValue{},
			}
		}
		return rows[key]
	}

	attachHistoryExportObservations(rows, []exportPingObservation{
		{timestamp: base.Add(2 * time.Second), taskID: "1", value: 102},
		{timestamp: base.Add(1 * time.Second), taskID: "1", value: 101},
		{timestamp: base.Add(4 * time.Second), taskID: "1", value: 0, loss: true},
		{timestamp: base.Add(3 * time.Second), taskID: "1", value: 1, loss: true},
		{timestamp: base.Add(31 * time.Second), taskID: "1", value: 103},
	}, rowFor)

	first := rows[base.UnixNano()].ping["1"]
	if first == nil || len(first.latency) != 1 || first.latency[0] != 102 || len(first.loss) != 1 || first.loss[0] != 0 {
		t.Fatalf("first slot = %#v, want one first-seen latency and loss", first)
	}
	second := rows[base.Add(30*time.Second).UnixNano()].ping["1"]
	if second == nil || len(second.latency) != 1 || second.latency[0] != 103 {
		t.Fatalf("second slot = %#v, want one latency", second)
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

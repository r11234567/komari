package historyexport

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

func TestSampleSpoolUnitIsAtomicAndReplayable(t *testing.T) {
	for _, boundary := range []string{"sample-unit-before-commit", "sample-unit-after-commit"} {
		t.Run(boundary, func(t *testing.T) {
			w, j := workerFixture(t, "sample30s")
			hit := false
			w.Fault = func(stage string) error {
				if stage == boundary {
					hit = true
					return errors.New("power loss")
				}
				return nil
			}
			if e := w.Step(context.Background(), j); e != nil {
				t.Fatal(e)
			}
			if !hit {
				t.Fatal("boundary not reached")
			}
			db, e := sql.Open("sqlite3", w.Queue.Path(j.ID, ".spool.db"))
			if e != nil {
				t.Fatal(e)
			}
			var n int
			e = db.QueryRow("SELECT COUNT(*) FROM samples").Scan(&n)
			db.Close()
			if e != nil {
				t.Fatal(e)
			}
			if boundary == "sample-unit-before-commit" && n != 0 {
				t.Fatal("unverified candidates committed", n)
			}
			if boundary == "sample-unit-after-commit" && n != 2 {
				t.Fatal("verified candidates lost", n)
			}
			w.Fault = nil
			if e = w.Queue.Control(context.Background(), j.Owner, j.ID, "retry"); e != nil {
				t.Fatal(e)
			}
			j, e = w.Queue.Get(context.Background(), j.Owner, j.ID)
			if e != nil {
				t.Fatal(e)
			}
			j = finishWorker(t, w, j)
			if rows := readCSV(t, w, j); len(rows) != 3 || rows[1][2] != "10" {
				t.Fatal("spool replay changed samples", rows)
			}
		})
	}
}

func TestSampleWideRowsResumeAcross128RowCheckpoint(t *testing.T) {
	for _, boundary := range []string{"sample-row-before-checkpoint", "sample-row-after-checkpoint"} {
		t.Run(boundary, func(t *testing.T) {
			w, j := workerFixture(t, "sample30s")
			ctx := context.Background()
			s, e := metric.Open(ctx, metric.SQLite(j.Source.DSN, metric.WithTablePrefix(j.Source.Prefix), metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
			if e != nil {
				t.Fatal(e)
			}
			tel, e := s.OpenTelemetry(ctx, j.Source.Telemetry)
			if e != nil {
				t.Fatal(e)
			}
			at := j.Start.Add(-3 * time.Hour)
			points := make([]metric.Point, 256)
			for i := range points {
				points[i] = metric.Point{MetricName: "cpu", EntityID: j.UUID, Timestamp: at.Add(time.Duration(i) * 30 * time.Second), Value: float64(i)}
			}
			if _, e = tel.Accept(ctx, metric.TelemetryBatch{Source: j.UUID, Identity: "wide-rows", Points: points}, time.Now()); e != nil {
				t.Fatal(e)
			}
			tel.Close()
			s.Close()
			j.Start = at
			if e = w.Queue.Save(ctx, j); e != nil {
				t.Fatal(e)
			}
			hit := false
			w.Fault = func(stage string) error {
				if stage == boundary && !hit {
					hit = true
					return errors.New("wide-row crash")
				}
				return nil
			}
			for i := 0; i < 20 && !hit; i++ {
				if e = w.Step(ctx, j); e != nil {
					t.Fatal(e)
				}
				j, e = w.Queue.Get(ctx, j.Owner, j.ID)
				if e != nil {
					t.Fatal(e)
				}
			}
			if !hit || j.Status != "failed" {
				t.Fatalf("not exercised: %+v", j)
			}
			if boundary == "sample-row-after-checkpoint" && (j.Committed.Rows != 128 || !j.Committed.SampleOutput) {
				t.Fatalf("missing row checkpoint: %+v", j.Committed)
			}
			f, e := os.OpenFile(w.Queue.Path(j.ID, ".csv.part"), os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				t.Fatal(e)
			}
			f.WriteString("unconfirmed tail\n")
			f.Close()
			w.Fault = nil
			if e = w.Queue.Control(ctx, j.Owner, j.ID, "retry"); e != nil {
				t.Fatal(e)
			}
			j, e = w.Queue.Get(ctx, j.Owner, j.ID)
			if e != nil {
				t.Fatal(e)
			}
			j = finishWorker(t, w, j)
			rows := readCSV(t, w, j)
			if len(rows) != 259 {
				t.Fatal("lost or duplicated sampled rows", len(rows))
			}
			seen := map[string]bool{}
			for _, row := range rows[1:] {
				if seen[row[1]] {
					t.Fatal("duplicate slot", row)
				}
				seen[row[1]] = true
				if strings.Contains(strings.Join(row, ","), "unconfirmed") {
					t.Fatal("tail retained")
				}
			}
		})
	}
}

func TestLegacyCSVUnitsGPUAndPingHeaders(t *testing.T) {
	checks := map[string]struct {
		value float64
		tags  map[string]string
		want  string
	}{
		"memory.used": {2 << 30, nil, "2.000"}, "net.in.rate": {2048, nil, "2.00"}, "traffic.up": {3 << 20, nil, "3.000"},
		"gpu.memory.used": {1 << 30, map[string]string{"device_name": "GPU test"}, "GPU test: 1.000"},
	}
	for _, c := range legacyMetricColumns {
		if check, ok := checks[c.metric]; ok {
			if got := c.value(check.value, check.tags); got != check.want {
				t.Fatalf("%s %s != %s", c.metric, got, check.want)
			}
		}
	}
	db, e := sql.Open("sqlite3", ":memory:")
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(`CREATE TABLE samples(metric TEXT,tags TEXT); INSERT INTO samples VALUES('ping.latency_ms','{"task_id":"8"}')`); e != nil {
		t.Fatal(e)
	}
	cols, grouped, e := legacyColumns(context.Background(), db, &Job{Request: Request{Metrics: []string{"ping.latency_ms", "ping.loss"}, PingNames: map[string]string{"7": "Tokyo"}}})
	if e != nil || !grouped || len(cols) != 4 || cols[0].group != "Tokyo" || cols[2].group != "Deleted ping task 8" {
		t.Fatal(cols, grouped, e)
	}
	if cols[1].value(.25, nil) != "25.00" || cols[0].value(-1, nil) != "" {
		t.Fatal("invalid loss/timeout compatibility")
	}
}

package metric

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This opt-in matrix uses a raw observation oracle, not per-bucket averages
// reconstructed from the summaries under test. Its report is separate from
// sustained worker qualification and contains no operator database content.
func TestTelemetryIndependentHistoricalMatrix(t *testing.T) {
	out := os.Getenv("KOMARI_TELEMETRY_INDEPENDENT_OUTPUT")
	if out == "" {
		t.Skip("set KOMARI_TELEMETRY_INDEPENDENT_OUTPUT for the offline matrix")
	}
	ctx := context.Background()
	now := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	var report []map[string]any
	for _, mode := range []string{"online", "migrated"} {
		for _, hours := range []int{1, 6, 12, 24, 168, 360, 720, 1080, 1440, 1800, 2160, 2520, 2880, 4320, 8760, 9120} {
			t.Run(fmt.Sprintf("%s/%dh", mode, hours), func(t *testing.T) {
				now := now
				dir := t.TempDir()
				s, e := telemetryFixture(t, dir)
				e.cfg.MaxLateness = 400 * 24 * time.Hour
				cfg := e.cfg
				for _, name := range []string{"cpu", sqliteMergedPingLatencyMetric} {
					if err := s.CreateMetric(ctx, Definition{Name: name, RetentionDays: 400}); err != nil {
						t.Fatal(err)
					}
				}
				span := time.Duration(hours) * time.Hour
				points := 600
				requestedInterval := TelemetryChartInterval(span, points)
				// A fixed historical window has aged beyond the current preset.
				// Request a grid that contains its oldest retained physical cells.
				interval := telemetryLCM(requestedInterval, telemetryResolution(time.Since(now.Add(-span))+time.Hour))
				// Preset retention is measured from the current query end. A
				// deliberately older 1h request no longer promises ten-second
				// detail: that belongs to the explicit precision-gap tests.
				end := alignTime(now, interval)
				now = end
				start := end.Add(-span)
				// Align both bounds to the actual epoch grid, including 7h output.
				start = alignTime(start, interval)
				var raw []Point
				for bucket, index := start, 0; bucket.Before(end); bucket, index = bucket.Add(interval), index+1 {
					for i := 0; i < 3; i++ {
						at := bucket.Add(time.Duration(i)*interval/3 + time.Second)
						value := float64((index*17+i*29)%131) + float64(i)/4
						raw = append(raw, Point{MetricName: "cpu", EntityID: "node", Timestamp: at, Value: value})
						ping := float64(10 + (index*11+i*7)%90)
						if (index+i)%7 == 0 {
							ping = -1
						}
						raw = append(raw, Point{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node", Tags: map[string]string{"task_id": "1"}, Timestamp: at, Value: ping})
					}
				}
				if mode == "migrated" {
					if err := s.WriteBatch(ctx, raw); err != nil {
						t.Fatal(err)
					}
					if _, err := s.db.Exec("INSERT INTO "+e.table("progress")+"(task,value) VALUES('legacy_migration_now',?)", now.UnixNano()); err != nil {
						t.Fatal(err)
					}
					if err := e.Close(); err != nil {
						t.Fatal(err)
					}
					if err := s.MigrateLegacy(ctx, cfg, nil); err != nil {
						t.Fatal(err)
					}
					if err := s.EnableTelemetry(ctx, cfg); err != nil {
						t.Fatal(err)
					}
					e = s.Telemetry()
				} else {
					for offset := 0; offset < len(raw); offset += 240 {
						if _, err := e.Accept(ctx, TelemetryBatch{Source: "node", Identity: fmt.Sprint(offset), Points: raw[offset:min(offset+240, len(raw))]}, now); err != nil {
							t.Fatal(err)
						}
					}
				}
				summarizeAll(t, e)
				if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				// Another immutable owner contributes inside an existing bucket.
				late := Point{MetricName: "cpu", EntityID: "node", Timestamp: alignTime(start.Add(span/2), interval).Add(2 * time.Second), Value: 211.5}
				if _, err := e.Accept(ctx, TelemetryBatch{Source: "node", Identity: "late-independent", Points: []Point{late}}, now); err != nil {
					t.Fatal(err)
				}
				raw = append(raw, late)
				summarizeAll(t, e)
				if _, err := e.SealDue(ctx, now.Add(2*time.Hour)); err != nil {
					t.Fatal(err)
				}
				promoteTelemetryAll(t, e, now)
				for {
					n, err := e.PromoteLegacyChunk(ctx, now, 512)
					if err != nil {
						t.Fatal(err)
					}
					if n == 0 {
						break
					}
				}
				for {
					more, err := e.ArchiveNext(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if !more {
						break
					}
				}
				for {
					// Receipt-time raw retention must elapse after ingestion.
					n, err := e.DeleteArchivedChunk(ctx, now.Add(time.Hour), 512)
					if err != nil {
						t.Fatal(err)
					}
					if n == 0 {
						break
					}
				}
				for _, name := range []string{"cpu", sqliteMergedPingLatencyMetric} {
					for {
						n, err := e.PruneSummaries(ctx, name, 400*24*time.Hour, now, 512)
						if err != nil {
							t.Fatal(err)
						}
						if n == 0 {
							break
						}
					}
				}
				if n := telemetryCount(t, e, "hot"); n != 0 {
					t.Fatal("raw handoff incomplete", n)
				}
				if err := e.root.Close(); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, e = telemetryFixture(t, dir)
				if err := e.root.Close(); err != nil {
					t.Fatal(err)
				}
				if n := telemetryCount(t, e, "hot"); n != 0 {
					t.Fatal("raw reappeared after restart", n)
				}
				row := map[string]any{"mode": mode, "hours": hours, "requested_interval_seconds": requestedInterval.Seconds(), "interval_seconds": interval.Seconds(), "raw_points": len(raw), "hot_rows_after_restart": 0, "cold_root_closed": true}
				for _, name := range []string{"cpu", sqliteMergedPingLatencyMetric} {
					var selected []Point
					for _, p := range raw {
						if p.MetricName == name {
							selected = append(selected, p)
						}
					}
					q := AggregateQuery{Query: Query{MetricName: name, EntityID: "node", Start: start, End: end.Add(-time.Nanosecond)}, Interval: interval, PreserveSeries: true}
					for _, agg := range []Aggregation{AggCount, AggSum, AggAvg, AggMin, AggMax, AggLast} {
						q.Aggregation = agg
						population := selected
						validCounts := map[int64]int{}
						if name == sqliteMergedPingLatencyMetric {
							var valid []Point
							for _, p := range selected {
								if p.Value >= 0 {
									valid = append(valid, p)
									validCounts[alignTime(p.Timestamp, q.Interval).UnixNano()]++
								}
							}
							if agg != AggCount && agg != AggLast {
								population = valid
							}
						}
						want, err := AggregatePoints(population, q)
						if err != nil {
							t.Fatal(err)
						}
						got, err := e.Chart(WithTelemetryEntities(ctx, []string{"node"}), q, now)
						if err != nil || len(got.Gaps) != 0 || len(got.Points) != len(want) || len(got.Points) == 0 || len(got.Points) > 600 {
							t.Fatalf("%s/%s points=%d/%d gaps=%d err=%v", name, agg, len(got.Points), len(want), len(got.Gaps), err)
						}
						for i, p := range got.Points {
							if name == sqliteMergedPingLatencyMetric && agg != AggCount {
								want[i].Count = validCounts[want[i].Bucket.UnixNano()]
							}
							if !p.Bucket.Equal(want[i].Bucket) || p.Count != want[i].Count || !independentClose(p.Value, want[i].Value) {
								t.Fatalf("%s/%s bucket %d got=%+v raw=%+v", name, agg, i, p, want[i])
							}
						}
						row[name+"_buckets"] = len(got.Points)
					}
					if name == sqliteMergedPingLatencyMetric {
						stats, err := e.PingSummary(ctx, q, now)
						if err != nil {
							t.Fatal(err)
						}
						var valid []Point
						loss := 0
						for _, p := range selected {
							if p.Value < 0 {
								loss++
							} else {
								valid = append(valid, p)
							}
						}
						ref, err := CalculateStats(valid)
						if err != nil {
							t.Fatal(err)
						}
						if len(stats.StdDev)+len(stats.P50)+len(stats.P99) != 0 {
							t.Fatal("retired statistics were recreated")
						}
						for _, view := range []struct {
							name      string
							got       []AggregatePoint
							want      float64
							tolerance float64
							count     int
						}{
							{"avg", stats.Avg, ref.Avg, 1e-8, len(valid)}, {"min", stats.Min, ref.Min, 1e-8, len(valid)}, {"max", stats.Max, ref.Max, 1e-8, len(valid)},
							{"loss", stats.Loss, float64(loss) / float64(len(selected)), 1e-8, len(selected)},
						} {
							if len(view.got) != 1 || view.got[0].Count != view.count || math.Abs(view.got[0].Value-view.want) > view.tolerance {
								t.Fatalf("Ping %s got=%+v want=%v attempts=%d", view.name, view.got, view.want, len(selected))
							}
						}
						row["ping_attempts"] = len(selected)
						row["ping_failures"] = loss
					}
				}
				report = append(report, row)
				t.Logf("verified %s/%dh: interval=%s raw=%d CPU/Ping buckets=%v/%v, hot=0, cold root closed", mode, hours, interval, len(raw), row["cpu_buckets"], row[sqliteMergedPingLatencyMetric+"_buckets"])
			})
		}
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(map[string]any{"passed": !t.Failed(), "windows": report}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(out, "historical-window-matrix.json"), append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func independentClose(a, b float64) bool { return math.Abs(a-b) <= 1e-8*math.Max(1, math.Abs(b)) }

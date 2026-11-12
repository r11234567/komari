package metric

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTelemetryStatsSealedEncodingPreservesEveryBit(t *testing.T) {
	for _, metric := range []string{"cpu", sqliteMergedPingLatencyMetric} {
		for _, count := range []int{0, 1, 17, 180, 10000} {
			t.Run(fmt.Sprintf("%s/%d", metric, count), func(t *testing.T) {
				var s telemetryStats
				at := time.Now().UTC()
				for i := 0; i < count; i++ {
					v := math.Sin(float64(i)) * 7
					if metric == sqliteMergedPingLatencyMetric && i%7 == 0 {
						v = -1
					}
					s.add(Point{MetricName: metric, Timestamp: at.Add(time.Duration(i) * time.Second), Value: v}, 17, i)
				}
				plain, err := s.encode()
				if err != nil {
					t.Fatal(err)
				}
				sealed, err := s.encodeSealed()
				if err != nil {
					t.Fatal(err)
				}
				var got telemetryStats
				if err = json.Unmarshal([]byte(sealed), &got); err != nil {
					t.Fatal(err)
				}
				restored, err := got.encode()
				if err != nil || restored != plain {
					t.Fatalf("statistics or digest changed: %v", err)
				}
				if len(sealed) > len(plain) {
					t.Fatal("storage grew")
				}
				var legacy telemetryStats
				if err = json.Unmarshal([]byte(plain), &legacy); err != nil {
					t.Fatal(err)
				}
				if err = got.merge(legacy); err != nil {
					t.Fatal(err)
				}
				if got.Attempts != 2*s.Attempts || got.N != 2*s.N || got.Loss != 2*s.Loss {
					t.Fatal("mixed-codec merge lost attempts")
				}
			})
		}
	}
}

func TestTelemetryStatsCodecRejectsCorruption(t *testing.T) {
	for _, raw := range []string{`"TSC2:AAAA"`, `"TSC1:invalid"`, `"TSC1:AAAA"`, `"TSC1:` + strings.Repeat("A", 2<<20) + `"`, `{`} {
		var s telemetryStats
		if err := json.Unmarshal([]byte(raw), &s); err == nil {
			t.Fatal("corrupt envelope accepted")
		}
	}
	encoder, err := statsEncoder()
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{7, telemetryStatsLimit + 1} {
		statsEncodeMu.Lock()
		z := encoder.EncodeAll(make([]byte, size), nil)
		statsEncodeMu.Unlock()
		encoded := `"` + telemetryStatsPrefix + base64.StdEncoding.EncodeToString(z) + `"`
		var s telemetryStats
		if err = json.Unmarshal([]byte(encoded), &s); err == nil {
			t.Fatal("malformed or oversized decoded payload accepted")
		}
	}
}

func statsSemanticHash(t *testing.T, e *Telemetry) (string, int64) {
	t.Helper()
	r, err := e.s.db.Query("SELECT segment_id,series_id,resolution,bucket,stats FROM " + e.table("summary") + " ORDER BY segment_id,series_id,resolution,bucket")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	h := sha256.New()
	var size int64
	for r.Next() {
		var segment, series, raw string
		var resolution, bucket int64
		if err = r.Scan(&segment, &series, &resolution, &bucket, &raw); err != nil {
			t.Fatal(err)
		}
		var s telemetryStats
		if err = json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatal(err)
		}
		canonical, err := s.encode()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%s/%s/%d/%d/%s\n", segment, series, resolution, bucket, canonical)
		size += int64(len(raw))
	}
	if err = r.Err(); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil)), size
}

func TestTelemetryStatsCompactionBoundedRestartRollback(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 5; i++ {
		if _, err := e.Accept(ctx, telemetryBatch(fmt.Sprint(i), now.Add(time.Duration(i)*20*time.Second), 1, 2, 3, 4), now); err != nil {
			t.Fatal(err)
		}
	}
	summarizeAll(t, e)
	before, bytesBefore := statsSemanticHash(t, e)
	if n, _, err := e.CompactStatsChunk(ctx, "", 2); err != nil || n != 0 {
		t.Fatal("mutable cells compacted", n, err)
	}
	if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.cfg.Fault = func(stage string) error {
		if stage == "stats-before-compaction-commit" {
			return errors.New("crash")
		}
		return nil
	}
	if _, _, err := e.CompactStatsChunk(ctx, "", 2); err == nil {
		t.Fatal("fault not reached")
	}
	if hash, n := statsSemanticHash(t, e); hash != before || n != bytesBefore {
		t.Fatal("failed page committed")
	}
	e.cfg.Fault = nil
	n, cursor, err := e.CompactStatsChunk(ctx, "", 2)
	if err != nil || n != 2 || len(cursor) > 255 {
		t.Fatal(n, cursor, err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = s.OpenTelemetry(ctx, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for i := 0; i < 10; i++ {
		n, cursor, err = e.CompactStatsChunk(ctx, cursor, 2)
		if err != nil || n > 2 {
			t.Fatal(n, err)
		}
		if n == 0 {
			break
		}
		if i == 9 {
			t.Fatal("cursor did not finish")
		}
	}
	if hash, n := statsSemanticHash(t, e); hash != before || n >= bytesBefore {
		t.Fatal("compaction lost exact statistics or did not shrink", n, bytesBefore)
	}
	if _, _, err = e.CompactStatsChunk(ctx, "invalid", 2); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}

// Opt-in finite incident-copy experiment. The caller must create a disposable
// directory with an explicit marker; never point this at a live installation.
func TestTelemetryStatsCompactionIncidentCopy(t *testing.T) {
	dir := os.Getenv("KOMARI_TELEMETRY_COMPACTION_COPY")
	if dir == "" {
		t.Skip("disposable incident copy not supplied")
	}
	if _, err := os.Stat(filepath.Join(dir, "DISPOSABLE-CAPACITY-COPY")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	s, err := Open(ctx, SQLite(filepath.Join(dir, "metrics.db"), WithTablePrefix("metric_"), WithAutoMigrate(false), WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e, err := s.OpenTelemetry(ctx, TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	before, bytesBefore := statsSemanticHash(t, e)
	start := time.Now()
	cursor := ""
	total := 0
	for {
		n, next, err := e.CompactStatsChunk(ctx, cursor, 128)
		if err != nil {
			t.Fatal(err)
		}
		total += n
		cursor = next
		if n == 0 {
			break
		}
	}
	after, bytesAfter := statsSemanticHash(t, e)
	if after != before {
		t.Fatal("incident statistics changed")
	}
	result := map[string]any{"rows": total, "seconds": time.Since(start).Seconds(), "stats_before_bytes": bytesBefore, "stats_after_bytes": bytesAfter, "canonical_sha256_before": before, "canonical_sha256_after": after}
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "compaction.json"), append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log(string(b))
}

//go:build linux

package metric

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

type independentArchiveCase struct {
	Name               string
	Snapshot           ExportSnapshot
	Points, PlainBytes int64
	SHA256             string
}

// Preparation deliberately runs in another process. Otherwise compressor and
// fixture allocation peaks would be incorrectly reported as decoder RSS.
func TestTelemetryIndependentArchivePrepare(t *testing.T) {
	dir := os.Getenv("KOMARI_TELEMETRY_ARCHIVE_PREPARE")
	if dir == "" {
		t.Skip("opt-in isolated archive resource experiment")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var cases []independentArchiveCase
	for _, spec := range []struct {
		name               string
		points, labelBytes int
		legacy             bool
	}{
		{"small_repetitive_frames", 65536, 0, false},
		{"entropy_labels_frames", 16384, 3072, false},
		{"near_record_limit_frames", 1024, 63000, false},
		{"legacy_single_stream", 65536, 0, true},
	} {
		id := fmt.Sprintf("%x", sha256.Sum256([]byte(spec.name)))[:32]
		file, err := os.Create(filepath.Join(dir, id+".jsonl.zst"))
		if err != nil {
			t.Fatal(err)
		}
		compressed := &telemetryHashWriter{w: file, h: sha256.New()}
		writer, err := newTelemetryFrameWriter(compressed, id)
		if err != nil {
			t.Fatal(err)
		}
		var legacy *zstd.Encoder
		content := sha256.New()
		var encoder *json.Encoder
		if spec.legacy {
			legacy, err = zstd.NewWriter(compressed, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(1<<20))
			if err != nil {
				t.Fatal(err)
			}
			encoder = json.NewEncoder(io.MultiWriter(legacy, content))
			if err = encoder.Encode(telemetryRecord{Kind: "header", Version: 1, Segment: id}); err != nil {
				t.Fatal(err)
			}
		}
		start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		digest := sha256.New()
		rng := rand.New(rand.NewSource(17))
		var plain int64
		for i := 0; i < spec.points; i++ {
			name := "cpu"
			if i%2 == 1 {
				name = "memory"
			}
			p := Point{MetricName: name, EntityID: "node", Timestamp: start.Add(time.Duration(i) * time.Second), Value: float64(i % 101), Tags: map[string]string{"device": "0"}}
			if spec.labelBytes > 0 {
				b := make([]byte, (spec.labelBytes+1)/2)
				if _, err = rng.Read(b); err != nil {
					t.Fatal(err)
				}
				p.Labels = map[string]string{"description": hex.EncodeToString(b)[:spec.labelBytes]}
			}
			r := telemetryRecord{Kind: "point", Ingest: int64(i/128 + 1), Ordinal: i % 128, Received: start.UnixNano(), Point: &p}
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			plain += int64(len(b) + 1)
			if spec.legacy {
				err = encoder.Encode(r)
			} else {
				err = writer.add(r)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = json.NewEncoder(digest).Encode(p.normalized()); err != nil {
				t.Fatal(err)
			}
		}
		if spec.legacy {
			if err = json.NewEncoder(legacy).Encode(telemetryRecord{Kind: "footer", Count: int64(spec.points), Hash: hex.EncodeToString(content.Sum(nil))}); err != nil {
				t.Fatal(err)
			}
			err = legacy.Close()
			writer.encoder.Close()
		} else {
			err = writer.close()
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = file.Close(); err != nil {
			t.Fatal(err)
		}
		seg := telemetrySegment{ID: id, State: "archived", First: 1, Last: int64((spec.points-1)/128 + 1), Points: int64(spec.points), Hash: hex.EncodeToString(compressed.h.Sum(nil)), FileBytes: compressed.n}
		manifest := ExportSnapshot{Version: 1, Instance: "independent-resource", Watermark: seg.Last, Queries: []Query{{MetricName: "cpu", EntityID: "node", Start: start, End: start.Add(time.Duration(spec.points) * time.Second)}, {MetricName: "memory", EntityID: "node", Start: start, End: start.Add(time.Duration(spec.points) * time.Second)}}}
		if spec.legacy {
			manifest.Units = []ExportInput{{Segment: seg, File: id + ".jsonl.zst"}}
		} else {
			f, err := os.Open(filepath.Join(dir, id+".jsonl.zst"))
			if err != nil {
				t.Fatal(err)
			}
			idx, err := readTelemetryArchiveIndex(f, seg)
			f.Close()
			if err != nil || idx == nil {
				t.Fatal("missing independent frames", err)
			}
			for _, frame := range idx.Frames {
				part := seg
				part.Points, part.First, part.Last, part.FileBytes, part.Hash = frame.Points, frame.First, frame.Last, frame.Bytes, frame.Hash
				fr := frame
				manifest.Units = append(manifest.Units, ExportInput{Segment: part, File: id + ".jsonl.zst", Frame: &fr})
			}
		}
		cases = append(cases, independentArchiveCase{Name: spec.name, Snapshot: manifest, Points: int64(spec.points), PlainBytes: plain, SHA256: hex.EncodeToString(digest.Sum(nil))})
	}
	b, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "cases.json"), append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryIndependentArchiveRead(t *testing.T) {
	dir, name := os.Getenv("KOMARI_TELEMETRY_ARCHIVE_READ"), os.Getenv("KOMARI_TELEMETRY_ARCHIVE_CASE")
	if dir == "" || name == "" {
		t.Skip("run once per case in a fresh process")
	}
	b, err := os.ReadFile(filepath.Join(dir, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []independentArchiveCase
	if err = json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	var c independentArchiveCase
	for _, candidate := range cases {
		if candidate.Name == name {
			c = candidate
		}
	}
	if c.Name == "" {
		t.Fatal("unknown case", name)
	}
	ctx, cancel := context.WithTimeout(WithTelemetryEntities(context.Background(), []string{"node"}), 5*time.Minute)
	defer cancel()
	var before, after syscall.Rusage
	if err = syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	digest := sha256.New()
	var decoded, readBytes, emitted int64
	for i := range c.Snapshot.Units {
		err = VisitExportInputProgress(ctx, dir, c.Snapshot, i, func(n, bytes int64) { decoded += n; readBytes += bytes }, func(o ExportObservation) error { emitted++; return json.NewEncoder(digest).Encode(o.Point) })
		if err != nil {
			t.Fatal(err)
		}
	}
	wall := time.Since(start)
	if err = syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
		t.Fatal(err)
	}
	sha := hex.EncodeToString(digest.Sum(nil))
	if decoded != c.Points || emitted != c.Points || sha != c.SHA256 {
		t.Fatalf("population/hash mismatch decoded=%d emitted=%d expected=%d hash=%s/%s", decoded, emitted, c.Points, sha, c.SHA256)
	}
	seconds := func(v syscall.Timeval) float64 { return float64(v.Sec) + float64(v.Usec)/1e6 }
	status, _ := os.ReadFile("/proc/self/status")
	var peak string
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			peak = strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:"))
		}
	}
	report := map[string]any{"name": name, "points": emitted, "decoded_records": decoded, "queries": len(c.Snapshot.Queries), "frames_or_streams": len(c.Snapshot.Units), "plain_record_bytes": c.PlainBytes, "compressed_bytes_read": readBytes, "sha256": sha, "wall_seconds": wall.Seconds(), "user_cpu_seconds": seconds(after.Utime) - seconds(before.Utime), "system_cpu_seconds": seconds(after.Stime) - seconds(before.Stime), "process_maxrss_kib": after.Maxrss, "proc_vmhwm": peak, "gomaxprocs": runtime.GOMAXPROCS(0)}
	// Capture the effective controls from inside the measured process, rather
	// than inferring enforcement from systemd-run's command line alone.
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(cgroup), "\n") {
		if strings.HasPrefix(line, "0::") {
			base := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "0::"))
			for _, file := range []string{"cpu.max", "cpu.stat", "memory.max", "memory.peak", "memory.swap.max"} {
				if value, readErr := os.ReadFile(filepath.Join(base, file)); readErr == nil {
					report[file] = strings.TrimSpace(string(value))
				}
			}
		}
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, name+"-result.json"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log(string(data))
}

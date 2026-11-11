package historyexport

import (
	"math"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

// These observations never drive scheduling or substitute for the hard quota.
// A rate measures recent wall time under the actual quota, not available CPU.
type Throughput struct {
	At                   time.Time `json:"at"`
	WindowSeconds        float64   `json:"window_seconds"`
	RowsPerSecond        float64   `json:"rows_per_second"`
	OutputBytesPerSecond float64   `json:"output_bytes_per_second"`
	DecodedPerSecond     float64   `json:"decoded_records_per_second"`
}

type PhaseETA struct {
	State       string  `json:"state"`
	Scope       string  `json:"scope"`
	Reason      string  `json:"reason,omitempty"`
	LowSeconds  float64 `json:"low_seconds,omitempty"`
	HighSeconds float64 `json:"high_seconds,omitempty"`
}

type InputProgress struct {
	Index int    `json:"index"`
	Total int    `json:"total"`
	Kind  string `json:"kind"`
}

type progressCounters struct{ Rows, Bytes, Decoded, Units int64 }
type rateSample struct {
	Seconds              float64
	Rows, Bytes, Decoded int64
}
type unitSample struct {
	Seconds float64
	Units   int64
}

// Private, version-tolerant sampler state travels with the durable job. At
// most twelve samples are kept. Old jobs bootstrap without inventing history.
type progressState struct {
	At, RateAt, UnitAt     time.Time
	Phase, Status          string
	Counters, UnitCounters progressCounters
	Rates                  []rateSample
	Units                  []unitSample
	Comparable             bool
}

func activeStatus(s string) bool  { return s == "running" || s == "publishing" }
func waitingStatus(s string) bool { return s == "queued" || s == "paused" || s == "retry_wait" }
func (j *Job) counters() progressCounters {
	return progressCounters{j.WrittenRows, j.OutputBytes, j.DecodedRecords, j.PhaseCompleted}
}
func (j *Job) resetRates(now time.Time) {
	s := &j.observation
	s.RateAt, s.UnitAt, s.Counters, s.UnitCounters = now, now, j.counters(), j.counters()
	s.Rates, s.Units = nil, nil
	j.Throughput = Throughput{}
	j.PhaseETA = PhaseETA{State: "sampling", Scope: "phase"}
}

func (j *Job) observe(now time.Time) {
	now = now.UTC()
	s := &j.observation
	changed := s.Phase != j.Phase || j.PhaseStartedAt.IsZero()
	if !s.At.IsZero() && !now.Before(s.At) {
		dt := now.Sub(s.At).Seconds()
		if activeStatus(s.Status) && !changed {
			j.PhaseActiveSeconds += dt
		}
		if waitingStatus(s.Status) {
			j.WaitSeconds += dt
		}
	}
	if changed {
		j.PhaseStartedAt, j.PhaseActiveSeconds = now, 0
	}
	c := j.counters()
	rollback := c.Rows < s.Counters.Rows || c.Bytes < s.Counters.Bytes || c.Decoded < s.Counters.Decoded || c.Units < s.UnitCounters.Units
	if changed || s.Status != j.Status || s.At.IsZero() || rollback || now.Before(s.At) {
		j.resetRates(now)
	} else if activeStatus(j.Status) {
		if dt := now.Sub(s.RateAt).Seconds(); dt >= 5 {
			s.Rates = append(s.Rates, rateSample{dt, c.Rows - s.Counters.Rows, c.Bytes - s.Counters.Bytes, c.Decoded - s.Counters.Decoded})
			if len(s.Rates) > 12 {
				s.Rates = s.Rates[len(s.Rates)-12:]
			}
			s.RateAt, s.Counters = now, c
			var seconds float64
			var rows, bytes, decoded int64
			for _, r := range s.Rates {
				seconds += r.Seconds
				rows += r.Rows
				bytes += r.Bytes
				decoded += r.Decoded
			}
			j.Throughput = Throughput{now, seconds, float64(rows) / seconds, float64(bytes) / seconds, float64(decoded) / seconds}
		}
		if c.Units > s.UnitCounters.Units {
			s.Units = append(s.Units, unitSample{now.Sub(s.UnitAt).Seconds(), c.Units - s.UnitCounters.Units})
			if len(s.Units) > 12 {
				s.Units = s.Units[len(s.Units)-12:]
			}
			s.UnitAt, s.UnitCounters = now, c
		}
	}
	s.At, s.Phase, s.Status = now, j.Phase, j.Status
	if waitingStatus(j.Status) {
		if j.WaitingSince.IsZero() {
			j.WaitingSince = now
		}
	} else {
		j.WaitingSince = time.Time{}
	}
	j.estimateETA(now)
}

func (j *Job) estimateETA(now time.Time) {
	j.PhaseETA = PhaseETA{State: "unknown", Scope: "phase", Reason: "unmodelled_phase"}
	if !activeStatus(j.Status) {
		j.PhaseETA.Reason = j.Status
		j.Throughput = Throughput{}
		return
	}
	if !j.observation.Comparable || j.ProgressKind != "input_units" {
		j.PhaseETA.Reason = "heterogeneous_or_unknown_input"
		return
	}
	j.PhaseETA.State, j.PhaseETA.Reason = "sampling", "insufficient_completed_units"
	var seconds float64
	lo, hi := math.Inf(1), 0.0
	for _, s := range j.observation.Units {
		if s.Seconds <= 0 || s.Units <= 0 {
			continue
		}
		seconds += s.Seconds
		rate := float64(s.Units) / s.Seconds
		lo, hi = math.Min(lo, rate), math.Max(hi, rate)
	}
	if len(j.observation.Units) < 5 || seconds < 30 || lo <= 0 || math.IsInf(lo, 1) {
		return
	}
	if hi/lo > 2 {
		j.PhaseETA.State, j.PhaseETA.Reason = "unknown", "variable_throughput"
		return
	}
	// A slow current unit must invalidate an old optimistic estimate even when
	// no further checkpoint has committed. This is not a task-wide forecast.
	if now.Sub(j.observation.UnitAt).Seconds() > math.Max(30, 2/lo) {
		j.PhaseETA.State, j.PhaseETA.Reason = "unknown", "no_recent_checkpoint"
		return
	}
	left := j.PhaseTotal - j.PhaseCompleted
	if left <= 0 {
		j.PhaseETA.Reason = "phase_finishing"
		return
	}
	j.PhaseETA = PhaseETA{State: "stable", Scope: "phase", LowSeconds: float64(left) / hi * .75, HighSeconds: float64(left) / lo * 1.5}
}

// Estimate only comparable framed inputs. A legacy single stream, mixed
// codecs, or highly uneven record/byte populations have no reliable unit ETA.
// This uses manifest metadata, never an extra scan to manufacture a denominator.
func (j *Job) inputProgress(snapshot metric.ExportSnapshot, index int) {
	if len(snapshot.Units) == 0 {
		j.CurrentInput = nil
		j.observation.Comparable = false
		return
	}
	kind := "legacy_stream"
	if index < len(snapshot.Units) && snapshot.Units[index].Frame != nil {
		kind = "zstd_frame"
	}
	j.CurrentInput = &InputProgress{Index: min(index+1, len(snapshot.Units)), Total: len(snapshot.Units), Kind: kind}
	if j.observation.Phase == j.Phase && j.PhaseCompleted > 0 {
		return
	}
	var minPoints, minBytes int64 = math.MaxInt64, math.MaxInt64
	var maxPoints, maxBytes int64
	for _, u := range snapshot.Units {
		if u.Frame == nil || u.Segment.Points <= 0 || u.Segment.FileBytes <= 0 {
			j.observation.Comparable = false
			return
		}
		minPoints, maxPoints = min(minPoints, u.Segment.Points), max(maxPoints, u.Segment.Points)
		minBytes, maxBytes = min(minBytes, u.Segment.FileBytes), max(maxBytes, u.Segment.FileBytes)
	}
	j.observation.Comparable = maxPoints <= minPoints*2 && maxBytes <= minBytes*2
}

// Recovery has no measurements for the downtime. Preserve counters/timestamps
// but discard rate history and never count downtime as active processing.
func (j *Job) recovered(now time.Time) {
	j.observation.At = now
	j.observation.Status = "recovering"
	j.resetRates(now)
}

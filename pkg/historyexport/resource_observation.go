package historyexport

import (
	"math"
	"time"
)

type ResourceProof struct {
	At             string  `json:"at"`
	EffectiveCPUs  float64 `json:"effective_cpus"`
	WorkerCPUs     float64 `json:"worker_cpus"`
	Quota          int64   `json:"quota_us"`
	Period         int64   `json:"period_us"`
	MemoryMax      int64   `json:"memory_max"`
	Cgroup         string  `json:"cgroup"`
	InstanceCgroup string  `json:"instance_cgroup"`
}

// Measurements are explicitly group-scoped, including every child process.
// memory.current includes page cache; it is not the worker's process RSS.
type ResourceUsage struct {
	Scope               string    `json:"scope"`
	At                  time.Time `json:"at"`
	SampleSeconds       float64   `json:"sample_seconds"`
	CPUShare            *float64  `json:"cpu_share,omitempty"`
	EffectiveCPUs       float64   `json:"effective_cpus"`
	CPUThrottledSeconds float64   `json:"cpu_throttled_seconds"`
	MemoryCurrentBytes  uint64    `json:"memory_current_bytes"`
	IOBudgetWaiting     bool      `json:"io_budget_waiting"`
	IOBudgetWaitSeconds float64   `json:"io_budget_wait_seconds"`
}

type resourceCounters struct{ usage, throttled, memory uint64 }
type resourceSampler struct {
	at       time.Time
	capacity float64
	group    string
	previous resourceCounters
}

func (s *resourceSampler) sample(p ResourceProof, c resourceCounters, now time.Time) *ResourceUsage {
	u := &ResourceUsage{Scope: "worker_group", At: now.UTC(), EffectiveCPUs: p.EffectiveCPUs, CPUThrottledSeconds: float64(c.throttled) / 1e6, MemoryCurrentBytes: c.memory}
	if !s.at.IsZero() && now.After(s.at) && s.group == p.Cgroup && c.usage >= s.previous.usage && p.EffectiveCPUs > 0 && s.capacity > 0 {
		u.SampleSeconds = now.Sub(s.at).Seconds()
		// Use the lower capacity when a quota changed between observations.
		share := float64(c.usage-s.previous.usage) / 1e6 / u.SampleSeconds / math.Min(p.EffectiveCPUs, s.capacity)
		u.CPUShare = &share
	}
	s.at, s.capacity, s.group, s.previous = now, p.EffectiveCPUs, p.Cgroup, c
	return u
}

func (w *Worker) observeResources(p ResourceProof, now time.Time) *ResourceUsage {
	c, err := readResourceCounters(p.Cgroup)
	if err != nil {
		return nil
	} // Unknown is never reported as zero CPU usage.
	u := w.resources.sample(p, c, now)
	if w.io != nil {
		waiting, duration := w.io.observation()
		u.IOBudgetWaiting = waiting
		u.IOBudgetWaitSeconds = duration.Seconds()
	}
	return u
}

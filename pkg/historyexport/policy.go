package historyexport

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

type Policy struct {
	At       time.Time                   `json:"at"`
	State    metric.TelemetryExportState `json:"state"`
	Owners   map[string]bool             `json:"owners"`
	Nodes    map[string]bool             `json:"nodes"`
	Pressure string                      `json:"pressure,omitempty"`
}

var foregroundPressureUntil atomic.Int64

func ObserveForeground(elapsed time.Duration) {
	if elapsed > 2*time.Second {
		foregroundPressureUntil.Store(time.Now().Add(15 * time.Second).UnixNano())
	}
}
func ForegroundPressure() bool { return foregroundPressureUntil.Load() > time.Now().UnixNano() }
func WritePolicy(dir string, p Policy) error {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	p.At = time.Now().UTC()
	return liveJSON(filepath.Join(dir, "policy.json"), p)
}

var errExportInvalidated = errors.New("export authorization, source generation or history epoch changed; create a new task")

func (q *Queue) policy(j *Job) (string, error) {
	b, e := os.ReadFile(filepath.Join(q.Dir, "policy.json"))
	if e != nil {
		return "instance_state_unavailable", nil
	}
	if len(b) > 1<<20 {
		return "instance_state_unavailable", nil
	}
	var p Policy
	if e = json.Unmarshal(b, &p); e != nil || time.Since(p.At) > 20*time.Second || time.Until(p.At) > time.Second {
		return "instance_state_unavailable", nil
	}
	if !p.Owners[j.Owner] || !p.Nodes[j.UUID] || p.State.Instance != j.Source.Instance || p.State.Epoch != j.Source.Epoch {
		return "", errExportInvalidated
	}
	return p.Pressure, nil
}

// Capacity/foreground pauses are automatically reconsidered without starting
// decoding. User pauses remain controlled exclusively by the persisted command.
func (w *Worker) checkPolicy(ctx context.Context, j *Job) error {
	if w.Verify != nil && !w.RequirePolicy {
		return nil
	} // Process-boundary fixtures install explicit proof.
	reason, e := w.Queue.policy(j)
	if e != nil {
		return e
	}
	if reason != "" {
		return &resourcePause{reason}
	}
	return ctx.Err()
}

type resourcePause struct{ reason string }

func (e *resourcePause) Error() string { return e.reason }

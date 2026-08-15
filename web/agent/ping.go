package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	pingProbeLeaseDuration = 20 * time.Second
	pingProbeTTL           = 30 * time.Second
)

// PingProbeAssignment is a scheduled latency probe leased by a Connect Agent.
type PingProbeAssignment struct {
	AssignmentID string
	AgentID      string
	TaskID       uint
	Protocol     string
	Target       string
	LeaseExpires time.Time
	expires      time.Time
	leased       bool
}

var pingProbeQueue = struct {
	sync.Mutex
	assignments map[string]map[uint]*PingProbeAssignment
	completed   map[string]map[uint]string
	notify      map[string]chan struct{}
}{assignments: make(map[string]map[uint]*PingProbeAssignment), completed: make(map[string]map[uint]string), notify: make(map[string]chan struct{})}

// EnqueuePingProbe coalesces repeated schedules for the same Agent and task.
func EnqueuePingProbe(agentID string, taskID uint, protocol, target string) bool {
	if agentID == "" || taskID == 0 || target == "" {
		return false
	}
	pingProbeQueue.Lock()
	defer pingProbeQueue.Unlock()
	byTask := pingProbeQueue.assignments[agentID]
	if byTask == nil {
		byTask = make(map[uint]*PingProbeAssignment)
		pingProbeQueue.assignments[agentID] = byTask
	}
	now := time.Now().UTC()
	if existing := byTask[taskID]; existing != nil && existing.expires.After(now) {
		return true
	}
	byTask[taskID] = &PingProbeAssignment{
		AssignmentID: uuid.NewString(), AgentID: agentID, TaskID: taskID,
		Protocol: protocol, Target: target, expires: now.Add(pingProbeTTL),
	}
	if signal := pingProbeQueue.notify[agentID]; signal != nil {
		close(signal)
		delete(pingProbeQueue.notify, agentID)
	}
	return true
}

// WaitPingProbe waits for an available or expired assignment lease.
func WaitPingProbe(ctx context.Context, agentID string) (*PingProbeAssignment, error) {
	for {
		pingProbeQueue.Lock()
		now := time.Now().UTC()
		var nextWake time.Time
		for taskID, assignment := range pingProbeQueue.assignments[agentID] {
			if !assignment.expires.After(now) {
				delete(pingProbeQueue.assignments[agentID], taskID)
				continue
			}
			if !assignment.leased || !assignment.LeaseExpires.After(now) {
				assignment.leased = true
				assignment.LeaseExpires = now.Add(pingProbeLeaseDuration)
				copy := *assignment
				pingProbeQueue.Unlock()
				return &copy, nil
			}
			for _, wake := range []time.Time{assignment.LeaseExpires, assignment.expires} {
				if nextWake.IsZero() || wake.Before(nextWake) {
					nextWake = wake
				}
			}
		}
		if len(pingProbeQueue.assignments[agentID]) == 0 {
			delete(pingProbeQueue.assignments, agentID)
		}
		signal := pingProbeQueue.notify[agentID]
		if signal == nil {
			signal = make(chan struct{})
			pingProbeQueue.notify[agentID] = signal
		}
		pingProbeQueue.Unlock()

		var timer *time.Timer
		var wake <-chan time.Time
		if !nextWake.IsZero() {
			timer = time.NewTimer(time.Until(nextWake))
			wake = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil, ctx.Err()
		case <-signal:
			if timer != nil {
				timer.Stop()
			}
		case <-wake:
		}
	}
}

// ValidatePingProbeResult binds a result to its authenticated Agent and lease.
func ValidatePingProbeResult(agentID, assignmentID string, taskID uint) (bool, error) {
	pingProbeQueue.Lock()
	defer pingProbeQueue.Unlock()
	if pingProbeQueue.completed[agentID][taskID] == assignmentID {
		return true, nil
	}
	assignment := pingProbeQueue.assignments[agentID][taskID]
	if assignment == nil || assignment.AssignmentID != assignmentID || !assignment.expires.After(time.Now().UTC()) {
		return false, fmt.Errorf("ping probe assignment is unknown or expired")
	}
	return false, nil
}

// CompletePingProbe removes a successfully persisted assignment.
func CompletePingProbe(agentID, assignmentID string, taskID uint) {
	pingProbeQueue.Lock()
	defer pingProbeQueue.Unlock()
	byTask := pingProbeQueue.assignments[agentID]
	if assignment := byTask[taskID]; assignment != nil && assignment.AssignmentID == assignmentID {
		delete(byTask, taskID)
		completed := pingProbeQueue.completed[agentID]
		if completed == nil {
			completed = make(map[uint]string)
			pingProbeQueue.completed[agentID] = completed
		}
		completed[taskID] = assignmentID
		if len(byTask) == 0 {
			delete(pingProbeQueue.assignments, agentID)
		}
	}
}

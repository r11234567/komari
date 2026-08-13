package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

const returnRouteLeaseDuration = 45 * time.Second

// ReturnRouteAssignment is the transport-neutral assignment leased by a
// Connect Agent. Legacy v2 delivery remains in v2_events.go.
type ReturnRouteAssignment struct {
	AssignmentID string
	AgentID      string
	TaskID       uint
	Protocol     string
	Target       string
	IPVersion    int
	MaxHops      int
	LeaseExpires time.Time
	leased       bool
}

var returnRouteQueue = struct {
	sync.Mutex
	assignments map[string]map[uint]*ReturnRouteAssignment
	completed   map[string]map[uint]string
	notify      map[string]chan struct{}
}{assignments: make(map[string]map[uint]*ReturnRouteAssignment), completed: make(map[string]map[uint]string), notify: make(map[string]chan struct{})}

// EnqueueReturnRouteProbe coalesces repeated schedules for the same task.
func EnqueueReturnRouteProbe(agentID string, taskID uint, protocol, target string, ipVersion, maxHops int) bool {
	if agentID == "" || taskID == 0 || target == "" {
		return false
	}
	returnRouteQueue.Lock()
	defer returnRouteQueue.Unlock()
	byTask := returnRouteQueue.assignments[agentID]
	if byTask == nil {
		byTask = make(map[uint]*ReturnRouteAssignment)
		returnRouteQueue.assignments[agentID] = byTask
	}
	if _, exists := byTask[taskID]; exists {
		return true
	}
	byTask[taskID] = &ReturnRouteAssignment{
		AssignmentID: uuid.NewString(), AgentID: agentID, TaskID: taskID,
		Protocol: protocol, Target: target, IPVersion: ipVersion, MaxHops: maxHops,
	}
	if signal := returnRouteQueue.notify[agentID]; signal != nil {
		close(signal)
		delete(returnRouteQueue.notify, agentID)
	}
	return true
}

// WaitReturnRouteProbe waits for an available or expired assignment lease.
func WaitReturnRouteProbe(ctx context.Context, agentID string) (*ReturnRouteAssignment, error) {
	for {
		returnRouteQueue.Lock()
		now := time.Now().UTC()
		var nextExpiry time.Time
		for _, assignment := range returnRouteQueue.assignments[agentID] {
			if !assignment.leased || !assignment.LeaseExpires.After(now) {
				assignment.leased = true
				assignment.LeaseExpires = now.Add(returnRouteLeaseDuration)
				copy := *assignment
				returnRouteQueue.Unlock()
				return &copy, nil
			}
			if nextExpiry.IsZero() || assignment.LeaseExpires.Before(nextExpiry) {
				nextExpiry = assignment.LeaseExpires
			}
		}
		signal := returnRouteQueue.notify[agentID]
		if signal == nil {
			signal = make(chan struct{})
			returnRouteQueue.notify[agentID] = signal
		}
		returnRouteQueue.Unlock()
		var leaseExpired <-chan time.Time
		var timer *time.Timer
		if !nextExpiry.IsZero() {
			timer = time.NewTimer(time.Until(nextExpiry))
			leaseExpired = timer.C
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
		case <-leaseExpired:
		}
	}
}

// ValidateReturnRouteResult proves that a result belongs to its authenticated
// Agent and active task lease.
func ValidateReturnRouteResult(agentID, assignmentID string, taskID uint) (bool, error) {
	returnRouteQueue.Lock()
	defer returnRouteQueue.Unlock()
	if returnRouteQueue.completed[agentID][taskID] == assignmentID {
		return true, nil
	}
	assignment := returnRouteQueue.assignments[agentID][taskID]
	if assignment == nil || assignment.AssignmentID != assignmentID {
		return false, fmt.Errorf("return route assignment is unknown or expired")
	}
	return false, nil
}

// CompleteReturnRouteProbe removes a successfully persisted assignment.
func CompleteReturnRouteProbe(agentID, assignmentID string, taskID uint) {
	returnRouteQueue.Lock()
	defer returnRouteQueue.Unlock()
	byTask := returnRouteQueue.assignments[agentID]
	if assignment := byTask[taskID]; assignment != nil && assignment.AssignmentID == assignmentID {
		delete(byTask, taskID)
		completed := returnRouteQueue.completed[agentID]
		if completed == nil {
			completed = make(map[uint]string)
			returnRouteQueue.completed[agentID] = completed
		}
		completed[taskID] = assignmentID
		if len(byTask) == 0 {
			delete(returnRouteQueue.assignments, agentID)
		}
	}
}

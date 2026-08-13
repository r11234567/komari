package agent

import (
	"context"
	"testing"
	"time"
)

func resetReturnRouteQueue() {
	returnRouteQueue.Lock()
	defer returnRouteQueue.Unlock()
	returnRouteQueue.assignments = make(map[string]map[uint]*ReturnRouteAssignment)
	returnRouteQueue.completed = make(map[string]map[uint]string)
	returnRouteQueue.notify = make(map[string]chan struct{})
}

func TestReturnRouteAssignmentLifecycle(t *testing.T) {
	resetReturnRouteQueue()
	t.Cleanup(resetReturnRouteQueue)
	if !EnqueueReturnRouteProbe("node-a", 7, "icmp", "192.0.2.1", 4, 30) {
		t.Fatal("assignment was not queued")
	}
	assignment, err := WaitReturnRouteProbe(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if assignment.TaskID != 7 || assignment.AssignmentID == "" {
		t.Fatalf("unexpected assignment: %+v", assignment)
	}
	if _, err := ValidateReturnRouteResult("node-b", assignment.AssignmentID, 7); err == nil {
		t.Fatal("assignment accepted for a different agent")
	}
	if completed, err := ValidateReturnRouteResult("node-a", assignment.AssignmentID, 7); err != nil || completed {
		t.Fatal(err)
	}
	CompleteReturnRouteProbe("node-a", assignment.AssignmentID, 7)
	if completed, err := ValidateReturnRouteResult("node-a", assignment.AssignmentID, 7); err != nil || !completed {
		t.Fatalf("completed assignment retry was not accepted: completed=%v err=%v", completed, err)
	}
}

func TestReturnRouteLeaseIsRedeliveredAfterExpiry(t *testing.T) {
	resetReturnRouteQueue()
	t.Cleanup(resetReturnRouteQueue)
	EnqueueReturnRouteProbe("node-a", 8, "icmp", "192.0.2.2", 4, 30)
	first, err := WaitReturnRouteProbe(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	returnRouteQueue.Lock()
	returnRouteQueue.assignments["node-a"][8].LeaseExpires = time.Now().Add(10 * time.Millisecond)
	returnRouteQueue.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	second, err := WaitReturnRouteProbe(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if second.AssignmentID != first.AssignmentID {
		t.Fatalf("redelivery changed assignment ID: %q != %q", second.AssignmentID, first.AssignmentID)
	}
}

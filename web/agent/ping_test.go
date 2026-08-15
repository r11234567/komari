package agent

import (
	"context"
	"testing"
)

func resetPingProbeQueue() {
	pingProbeQueue.assignments = make(map[string]map[uint]*PingProbeAssignment)
	pingProbeQueue.completed = make(map[string]map[uint]string)
	pingProbeQueue.notify = make(map[string]chan struct{})
}

func TestPingProbeAssignmentLifecycle(t *testing.T) {
	resetPingProbeQueue()
	t.Cleanup(resetPingProbeQueue)
	if !EnqueuePingProbe("node-a", 7, "tcp", "example.com:80") {
		t.Fatal("enqueue failed")
	}
	assignment, err := WaitPingProbe(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if assignment.TaskID != 7 || assignment.Protocol != "tcp" || assignment.Target != "example.com:80" || assignment.AssignmentID == "" {
		t.Fatalf("unexpected assignment: %#v", assignment)
	}
	if _, err := ValidatePingProbeResult("node-b", assignment.AssignmentID, 7); err == nil {
		t.Fatal("cross-agent result was accepted")
	}
	if completed, err := ValidatePingProbeResult("node-a", assignment.AssignmentID, 7); err != nil || completed {
		t.Fatalf("active result validation: completed=%v err=%v", completed, err)
	}
	CompletePingProbe("node-a", assignment.AssignmentID, 7)
	if completed, err := ValidatePingProbeResult("node-a", assignment.AssignmentID, 7); err != nil || !completed {
		t.Fatalf("completed result retry: completed=%v err=%v", completed, err)
	}
}

package connectapi

import (
	"testing"
	"time"

	filev1 "github.com/r11234567/komari-proto/gen/go/komari/file/v1"
)

func TestFileStoreDispatchAndTake(t *testing.T) {
	store := &fileStore{
		agents:      make(map[string]chan *filev1.FileAssignment),
		assignments: make(map[string]*fileAssignment),
	}
	assignment := &filev1.FileAssignment{AssignmentId: "a-1", RequestId: "r-1"}
	if store.dispatch("agent-1", assignment) {
		t.Fatal("offline agent must not accept an assignment")
	}

	channel := make(chan *filev1.FileAssignment, 1)
	store.registerAgent("agent-1", channel)
	if !store.dispatch("agent-1", assignment) {
		t.Fatal("connected agent must accept an assignment")
	}
	select {
	case queued := <-channel:
		if queued.GetAssignmentId() != "a-1" {
			t.Fatalf("queued assignment = %q", queued.GetAssignmentId())
		}
	case <-time.After(time.Second):
		t.Fatal("assignment was not queued")
	}
	if got := store.takeAssignment("agent-1", "a-1"); got == nil || got.GetRequestId() != "r-1" {
		t.Fatalf("take assignment = %+v", got)
	}
	store.finishAssignment("a-1")
	if got := store.takeAssignment("agent-1", "a-1"); got != nil {
		t.Fatal("finished assignment still resolvable")
	}
}

func TestFileStoreRejectsWrongAgent(t *testing.T) {
	store := &fileStore{
		agents:      map[string]chan *filev1.FileAssignment{"agent-1": make(chan *filev1.FileAssignment, 1)},
		assignments: make(map[string]*fileAssignment),
	}
	if !store.dispatch("agent-1", &filev1.FileAssignment{AssignmentId: "a-2"}) {
		t.Fatal("dispatch failed")
	}
	if got := store.takeAssignment("agent-2", "a-2"); got != nil {
		t.Fatal("another agent resolved the assignment")
	}
}

func TestPathParent(t *testing.T) {
	cases := map[string]string{
		"/var/log":   "/var",
		"/":          "",
		"C:\\Users": "C:",
		"/tmp/":      "",
	}
	for input, want := range cases {
		if got := pathParent(input); got != want {
			t.Errorf("pathParent(%q) = %q, want %q", input, got, want)
		}
	}
}

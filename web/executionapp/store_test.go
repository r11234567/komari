package executionapp

import (
	"testing"
	"time"

	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	execv1 "github.com/r11234567/komari-proto/gen/go/komari/exec/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestNormalizeSpecAppliesBounds(t *testing.T) {
	spec, err := normalizeSpec(&execv1.ExecutionSpec{Command: " echo ok ", Timeout: durationpb.New(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Command != "echo ok" || spec.Timeout.AsDuration() != time.Minute || spec.MaxOutputBytes != defaultOutput {
		t.Fatalf("normalized spec = %#v", spec)
	}
	if _, err := normalizeSpec(&execv1.ExecutionSpec{Command: "echo", Timeout: durationpb.New(maximumTimeout + time.Second)}); err == nil {
		t.Fatal("oversized timeout was accepted")
	}
}

func TestExecutionStateTransitions(t *testing.T) {
	if !validTransition(commonv1.OperationState_OPERATION_STATE_QUEUED, commonv1.OperationState_OPERATION_STATE_RUNNING) {
		t.Fatal("queued execution cannot start")
	}
	if !validTransition(commonv1.OperationState_OPERATION_STATE_CANCEL_REQUESTED, commonv1.OperationState_OPERATION_STATE_CANCELLED) {
		t.Fatal("cancel-requested execution cannot finish cancelled")
	}
	if validTransition(commonv1.OperationState_OPERATION_STATE_SUCCEEDED, commonv1.OperationState_OPERATION_STATE_RUNNING) {
		t.Fatal("terminal execution restarted")
	}
}

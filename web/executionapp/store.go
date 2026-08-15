package executionapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/tasks"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	execv1 "github.com/r11234567/komari-proto/gen/go/komari/exec/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultTimeout    = 5 * time.Minute
	maximumTimeout    = 30 * time.Minute
	defaultOutput     = 1 << 20
	maximumOutput     = 8 << 20
	leaseDuration     = 30 * time.Second
	finishedRetention = 15 * time.Minute
	maximumTasks      = 256
	maximumCommand    = 64 << 10
	maximumArguments  = 128
)

var (
	ErrNotFound  = errors.New("execution not found")
	ErrForbidden = errors.New("execution is owned by another user")
	ErrInvalid   = errors.New("invalid execution event")
	ErrOutput    = errors.New("execution output limit exceeded")
	ErrTaskLimit = errors.New("too many active executions")
	ErrTerminal  = errors.New("execution already reached a terminal state")
)

type Task struct {
	mu sync.Mutex

	ID             string
	AgentID        string
	OwnerID        string
	AssignmentID   string
	IdempotencyKey string
	Spec           *execv1.ExecutionSpec
	Execution      *execv1.Execution
	LeaseExpiresAt time.Time
	Events         []*execv1.ExecutionEvent
	Sequence       uint64
	Output         strings.Builder
	notify         chan struct{}
}

type Dispatch struct {
	Assignment   *execv1.ExecutionAssignment
	Cancellation *execv1.ExecutionCancellation
}

var store = struct {
	sync.Mutex
	values map[string]*Task
	notify map[string]chan struct{}
}{values: make(map[string]*Task), notify: make(map[string]chan struct{})}

func Create(agentID, ownerID, idempotencyKey string, spec *execv1.ExecutionSpec) (*execv1.Execution, error) {
	result, err := CreateBatch([]string{agentID}, ownerID, idempotencyKey, spec)
	if err != nil {
		return nil, err
	}
	return result[0], nil
}

func CreateBatch(agentIDs []string, ownerID, idempotencyKey string, spec *execv1.ExecutionSpec) ([]*execv1.Execution, error) {
	ownerID = strings.TrimSpace(ownerID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if len(agentIDs) == 0 || ownerID == "" {
		return nil, errors.New("agent and owner are required")
	}
	normalized, err := normalizeSpec(spec)
	if err != nil {
		return nil, err
	}
	store.Lock()
	defer store.Unlock()
	pruneLocked(time.Now().UTC())
	results := make([]*execv1.Execution, 0, len(agentIDs))
	newTasks := make([]*Task, 0, len(agentIDs))
	databaseTasks := make([]tasks.CommandTask, 0, len(agentIDs))
	for _, rawAgentID := range agentIDs {
		agentID := strings.TrimSpace(rawAgentID)
		if agentID == "" {
			return nil, errors.New("agent ID is required")
		}
		if idempotencyKey != "" {
			found := false
			for _, existing := range store.values {
				if existing.AgentID == agentID && existing.OwnerID == ownerID && existing.IdempotencyKey == idempotencyKey {
					existing.mu.Lock()
					results = append(results, cloneExecution(existing.Execution))
					existing.mu.Unlock()
					found = true
					break
				}
			}
			if found {
				continue
			}
		}
		now := time.Now().UTC()
		id := uuid.NewString()
		task := &Task{
			ID: id, AgentID: agentID, OwnerID: ownerID, AssignmentID: uuid.NewString(), IdempotencyKey: idempotencyKey,
			Spec: cloneSpec(normalized), Execution: &execv1.Execution{ExecutionId: id, AgentId: agentID, State: commonv1.OperationState_OPERATION_STATE_QUEUED, CreatedAt: timestamppb.New(now)},
			notify: make(chan struct{}),
		}
		newTasks = append(newTasks, task)
		databaseTasks = append(databaseTasks, tasks.CommandTask{ID: id, Client: agentID, Command: displayCommand(normalized)})
	}
	if len(store.values)+len(newTasks) > maximumTasks {
		return nil, ErrTaskLimit
	}
	if err := tasks.CreateTaskBatch(databaseTasks); err != nil {
		return nil, err
	}
	for _, task := range newTasks {
		store.values[task.ID] = task
		results = append(results, cloneExecution(task.Execution))
		signalAgentLocked(task.AgentID)
	}
	return results, nil
}

func GetOwned(id, ownerID string) (*Task, error) {
	store.Lock()
	task := store.values[id]
	store.Unlock()
	if task == nil {
		return nil, ErrNotFound
	}
	if task.OwnerID != ownerID {
		return nil, ErrForbidden
	}
	task.expire(time.Now().UTC())
	return task, nil
}

func Get(id string) (*Task, error) {
	store.Lock()
	task := store.values[id]
	store.Unlock()
	if task == nil {
		return nil, ErrNotFound
	}
	task.expire(time.Now().UTC())
	return task, nil
}

func (task *Task) Snapshot() *execv1.Execution {
	task.mu.Lock()
	defer task.mu.Unlock()
	return cloneExecution(task.Execution)
}

func (task *Task) NextEvent(ctx context.Context, after uint64) (*execv1.ExecutionEvent, error) {
	for {
		task.expire(time.Now().UTC())
		task.mu.Lock()
		for _, event := range task.Events {
			if event.Sequence > after {
				result := cloneEvent(event)
				task.mu.Unlock()
				return result, nil
			}
		}
		if terminal(task.Execution.State) {
			task.mu.Unlock()
			return nil, ErrTerminal
		}
		signal := task.notify
		task.mu.Unlock()
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-signal:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func Cancel(id, reason string) (*execv1.Execution, error) {
	task, err := Get(id)
	if err != nil {
		return nil, err
	}
	task.mu.Lock()
	if terminal(task.Execution.State) || task.Execution.State == commonv1.OperationState_OPERATION_STATE_CANCEL_REQUESTED {
		result := cloneExecution(task.Execution)
		task.mu.Unlock()
		return result, nil
	}
	now := time.Now().UTC()
	if task.Execution.State == commonv1.OperationState_OPERATION_STATE_QUEUED {
		task.appendTerminalLocked(commonv1.OperationState_OPERATION_STATE_CANCELLED, nil, "CANCELLED", reason, now)
	} else {
		task.Execution.State = commonv1.OperationState_OPERATION_STATE_CANCEL_REQUESTED
		task.signalLocked()
	}
	result := cloneExecution(task.Execution)
	task.mu.Unlock()
	signalAgent(task.AgentID)
	return result, nil
}

func NextDispatch(ctx context.Context, agentID, afterAssignment string, delivered map[string]bool) (*Dispatch, error) {
	for {
		store.Lock()
		now := time.Now().UTC()
		pruneLocked(now)
		for _, task := range store.values {
			if task.AgentID != agentID {
				continue
			}
			task.mu.Lock()
			task.expireLocked(now)
			if task.Execution.State == commonv1.OperationState_OPERATION_STATE_CANCEL_REQUESTED && !delivered["cancel:"+task.ID] {
				delivered["cancel:"+task.ID] = true
				result := &Dispatch{Cancellation: &execv1.ExecutionCancellation{ExecutionId: task.ID, Reason: "cancel requested", RequestedAt: timestamppb.New(now)}}
				task.mu.Unlock()
				store.Unlock()
				return result, nil
			}
			assignable := task.Execution.State == commonv1.OperationState_OPERATION_STATE_QUEUED && !task.LeaseExpiresAt.After(now)
			if assignable && (task.AssignmentID != afterAssignment || !delivered["assign:"+task.AssignmentID]) {
				task.LeaseExpiresAt = now.Add(leaseDuration)
				delivered["assign:"+task.AssignmentID] = true
				result := &Dispatch{Assignment: &execv1.ExecutionAssignment{
					AssignmentId: task.AssignmentID, Execution: cloneExecution(task.Execution), LeaseExpiresAt: timestamppb.New(task.LeaseExpiresAt), Spec: cloneSpec(task.Spec),
				}}
				task.mu.Unlock()
				store.Unlock()
				return result, nil
			}
			task.mu.Unlock()
		}
		signal := store.notify[agentID]
		if signal == nil {
			signal = make(chan struct{})
			store.notify[agentID] = signal
		}
		store.Unlock()
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-signal:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func Report(agentID string, event *execv1.ExecutionEvent) (uint64, error) {
	if event == nil || strings.TrimSpace(event.ExecutionId) == "" || event.Sequence == 0 {
		return 0, ErrInvalid
	}
	task, err := Get(event.ExecutionId)
	if err != nil {
		return 0, err
	}
	if task.AgentID != agentID {
		return 0, ErrForbidden
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	if event.Sequence <= task.Sequence {
		for _, stored := range task.Events {
			if stored.Sequence == event.Sequence && proto.Equal(stored, event) {
				return task.Sequence, nil
			}
		}
		// A prior output response may have been lost after commit. Permit the
		// authenticated Agent's terminal update to advance to the next sequence
		// instead of leaving an otherwise completed process stuck as running.
		if !terminal(task.Execution.State) && terminal(event.State) {
			event.Sequence = task.Sequence + 1
		} else {
			return task.Sequence, nil
		}
	}
	if event.Sequence != task.Sequence+1 || terminal(task.Execution.State) {
		return task.Sequence, ErrInvalid
	}
	if uint64(len(event.Output))+task.Execution.OutputBytes > task.Spec.MaxOutputBytes {
		return task.Sequence, ErrOutput
	}
	currentState := task.Execution.State
	if !validTransition(currentState, event.State) {
		return task.Sequence, ErrInvalid
	}
	now := time.Now().UTC()
	if event.OccurredAt == nil || !event.OccurredAt.IsValid() {
		event.OccurredAt = timestamppb.New(now)
	}
	event.ExecutionId = task.ID
	if event.State == commonv1.OperationState_OPERATION_STATE_RUNNING && task.Execution.StartedAt == nil {
		task.Execution.StartedAt = timestamppb.New(now)
	}
	if event.State != commonv1.OperationState_OPERATION_STATE_UNSPECIFIED &&
		!(currentState == commonv1.OperationState_OPERATION_STATE_CANCEL_REQUESTED && event.State == commonv1.OperationState_OPERATION_STATE_RUNNING) {
		task.Execution.State = event.State
	}
	if len(event.Output) > 0 {
		task.Execution.OutputBytes += uint64(len(event.Output))
		task.Output.Write(event.Output)
	}
	stored := cloneEvent(event)
	task.Sequence = event.Sequence
	task.Events = append(task.Events, stored)
	if terminal(task.Execution.State) {
		task.Execution.FinishedAt = timestamppb.New(now)
		task.Execution.ExitCode = event.ExitCode
		exitCode := -1
		if event.ExitCode != nil {
			exitCode = int(*event.ExitCode)
		}
		_ = tasks.SaveTaskResult(task.ID, task.AgentID, task.Output.String(), exitCode, now)
	}
	task.signalLocked()
	return task.Sequence, nil
}

func (task *Task) expire(now time.Time) {
	task.mu.Lock()
	expired := task.expireLocked(now)
	task.mu.Unlock()
	if expired {
		signalAgent(task.AgentID)
	}
}

func (task *Task) expireLocked(now time.Time) bool {
	if terminal(task.Execution.State) {
		return false
	}
	created := task.Execution.CreatedAt.AsTime()
	if now.Before(created.Add(task.Spec.Timeout.AsDuration())) {
		return false
	}
	task.appendTerminalLocked(commonv1.OperationState_OPERATION_STATE_DEADLINE_EXCEEDED, nil, "DEADLINE_EXCEEDED", "execution deadline exceeded", now)
	return true
}

func (task *Task) appendTerminalLocked(state commonv1.OperationState, exitCode *int32, code, message string, now time.Time) {
	task.Sequence++
	event := &execv1.ExecutionEvent{ExecutionId: task.ID, Sequence: task.Sequence, OccurredAt: timestamppb.New(now), State: state, ExitCode: exitCode}
	if message != "" {
		event.Error = &commonv1.ErrorDetail{Code: code, Message: message}
	}
	task.Events = append(task.Events, event)
	task.Execution.State = state
	task.Execution.FinishedAt = timestamppb.New(now)
	task.Execution.ExitCode = exitCode
	_ = tasks.SaveTaskResult(task.ID, task.AgentID, task.Output.String()+message, -1, now)
	task.signalLocked()
}

func normalizeSpec(spec *execv1.ExecutionSpec) (*execv1.ExecutionSpec, error) {
	if spec == nil || strings.TrimSpace(spec.Command) == "" {
		return nil, errors.New("command is required")
	}
	if len(spec.Command) > maximumCommand || len(spec.Arguments) > maximumArguments {
		return nil, errors.New("command or argument list is too large")
	}
	timeout := defaultTimeout
	if spec.Timeout != nil {
		if err := spec.Timeout.CheckValid(); err != nil {
			return nil, err
		}
		timeout = spec.Timeout.AsDuration()
	}
	if timeout <= 0 || timeout > maximumTimeout {
		return nil, fmt.Errorf("execution timeout must be between 1s and %s", maximumTimeout)
	}
	maxOutput := spec.MaxOutputBytes
	if maxOutput == 0 {
		maxOutput = defaultOutput
	}
	if maxOutput > maximumOutput {
		return nil, fmt.Errorf("execution output exceeds %d bytes", maximumOutput)
	}
	result := cloneSpec(spec)
	result.Command = strings.TrimSpace(result.Command)
	result.Timeout = durationpb.New(timeout)
	result.MaxOutputBytes = maxOutput
	return result, nil
}

func displayCommand(spec *execv1.ExecutionSpec) string {
	if len(spec.Arguments) == 0 {
		return spec.Command
	}
	return strings.Join(append([]string{spec.Command}, spec.Arguments...), " ")
}

func terminal(state commonv1.OperationState) bool {
	return state == commonv1.OperationState_OPERATION_STATE_CANCELLED || state == commonv1.OperationState_OPERATION_STATE_DEADLINE_EXCEEDED ||
		state == commonv1.OperationState_OPERATION_STATE_FAILED || state == commonv1.OperationState_OPERATION_STATE_SUCCEEDED
}

func validTransition(current, next commonv1.OperationState) bool {
	if terminal(next) {
		return current == commonv1.OperationState_OPERATION_STATE_QUEUED || current == commonv1.OperationState_OPERATION_STATE_RUNNING || current == commonv1.OperationState_OPERATION_STATE_CANCEL_REQUESTED
	}
	switch current {
	case commonv1.OperationState_OPERATION_STATE_QUEUED:
		return next == commonv1.OperationState_OPERATION_STATE_RUNNING
	case commonv1.OperationState_OPERATION_STATE_RUNNING, commonv1.OperationState_OPERATION_STATE_CANCEL_REQUESTED:
		return next == commonv1.OperationState_OPERATION_STATE_RUNNING || next == commonv1.OperationState_OPERATION_STATE_UNSPECIFIED
	default:
		return false
	}
}

func cloneExecution(value *execv1.Execution) *execv1.Execution {
	if value == nil {
		return nil
	}
	return proto.Clone(value).(*execv1.Execution)
}

func cloneEvent(value *execv1.ExecutionEvent) *execv1.ExecutionEvent {
	return proto.Clone(value).(*execv1.ExecutionEvent)
}

func cloneSpec(value *execv1.ExecutionSpec) *execv1.ExecutionSpec {
	return proto.Clone(value).(*execv1.ExecutionSpec)
}

func (task *Task) signalLocked() {
	close(task.notify)
	task.notify = make(chan struct{})
}

func signalAgent(agentID string) {
	store.Lock()
	signalAgentLocked(agentID)
	store.Unlock()
}

func signalAgentLocked(agentID string) {
	if signal := store.notify[agentID]; signal != nil {
		close(signal)
		delete(store.notify, agentID)
	}
}

func pruneLocked(now time.Time) {
	for id, task := range store.values {
		task.mu.Lock()
		finished := task.Execution.FinishedAt
		remove := finished != nil && finished.IsValid() && now.Sub(finished.AsTime()) > finishedRetention
		task.mu.Unlock()
		if remove {
			delete(store.values, id)
		}
	}
}

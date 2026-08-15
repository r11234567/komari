package remotemanagement

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	websshv1 "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	leaseDuration       = 30 * time.Second
	maxSessionDuration  = 2 * time.Hour
	closedRetention     = 5 * time.Minute
	maxBufferedOutput   = 8 << 20
	maxActiveSessions   = 64
	maxBufferedCommands = 1024
)

var (
	ErrNotFound       = errors.New("remote session not found")
	ErrForbidden      = errors.New("remote session is owned by another user")
	ErrClosed         = errors.New("remote session is closed")
	ErrSequence       = errors.New("remote session sequence is invalid")
	ErrOutputLimit    = errors.New("remote session output limit exceeded")
	ErrSessionLimit   = errors.New("too many active remote sessions")
	ErrInvalidLease   = errors.New("remote session lease is unknown or expired")
	ErrCommandBacklog = errors.New("remote session command backlog is full")
)

type Session struct {
	mu sync.Mutex

	ID               string
	AgentID          string
	OwnerID          string
	AssignmentID     string
	CreatedAt        time.Time
	LeaseExpiresAt   time.Time
	Shell            string
	WorkingDirectory string
	Size             *websshv1.TerminalSize

	attached       bool
	closed         *websshv1.SessionClosed
	commandSeq     uint64
	agentSeq       uint64
	commands       []*websshv1.AttachSessionResponse
	events         []*websshv1.SessionEvent
	bufferedOutput uint64
	commandNotify  chan struct{}
	eventNotify    chan struct{}
}

var sessions = struct {
	sync.Mutex
	values map[string]*Session
	notify map[string]chan struct{}
}{values: make(map[string]*Session), notify: make(map[string]chan struct{})}

func Create(agentID, ownerID, shell, workingDirectory string, size *websshv1.TerminalSize) (*Session, error) {
	if agentID == "" || ownerID == "" {
		return nil, errors.New("agent and owner are required")
	}
	sessions.Lock()
	defer sessions.Unlock()
	pruneLocked(time.Now().UTC())
	if len(sessions.values) >= maxActiveSessions {
		return nil, ErrSessionLimit
	}
	now := time.Now().UTC()
	session := &Session{
		ID: uuid.NewString(), AgentID: agentID, OwnerID: ownerID, AssignmentID: uuid.NewString(),
		CreatedAt: now, Shell: shell, WorkingDirectory: workingDirectory, Size: cloneSize(size),
		commandNotify: make(chan struct{}), eventNotify: make(chan struct{}),
	}
	sessions.values[session.ID] = session
	if signal := sessions.notify[agentID]; signal != nil {
		close(signal)
		delete(sessions.notify, agentID)
	}
	return session, nil
}

func GetOwned(sessionID, ownerID string) (*Session, error) {
	sessions.Lock()
	session := sessions.values[sessionID]
	sessions.Unlock()
	if session == nil {
		return nil, ErrNotFound
	}
	if session.OwnerID != ownerID {
		return nil, ErrForbidden
	}
	return session, nil
}

func Lease(ctx context.Context, agentID string) (*websshv1.SessionAssignment, error) {
	for {
		sessions.Lock()
		now := time.Now().UTC()
		pruneLocked(now)
		var nextWake time.Time
		for _, session := range sessions.values {
			if session.AgentID != agentID {
				continue
			}
			session.mu.Lock()
			available := session.closed == nil && !session.attached && !session.LeaseExpiresAt.After(now)
			if available {
				session.LeaseExpiresAt = now.Add(leaseDuration)
				assignment := session.assignment()
				session.mu.Unlock()
				sessions.Unlock()
				return assignment, nil
			}
			if session.closed == nil && !session.attached && session.LeaseExpiresAt.After(now) && (nextWake.IsZero() || session.LeaseExpiresAt.Before(nextWake)) {
				nextWake = session.LeaseExpiresAt
			}
			session.mu.Unlock()
		}
		signal := sessions.notify[agentID]
		if signal == nil {
			signal = make(chan struct{})
			sessions.notify[agentID] = signal
		}
		sessions.Unlock()
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

func Attach(agentID, assignmentID, sessionID string) (*Session, error) {
	sessions.Lock()
	session := sessions.values[sessionID]
	sessions.Unlock()
	if session == nil {
		return nil, ErrNotFound
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.AgentID != agentID || session.AssignmentID != assignmentID || !session.LeaseExpiresAt.After(time.Now().UTC()) {
		return nil, ErrInvalidLease
	}
	if session.closed != nil {
		return nil, ErrClosed
	}
	if session.attached {
		return nil, ErrInvalidLease
	}
	session.attached = true
	return session, nil
}

func (s *Session) Detach() {
	s.mu.Lock()
	if s.closed == nil {
		s.attached = false
		s.LeaseExpiresAt = time.Time{}
	}
	s.mu.Unlock()
	sessions.Lock()
	if signal := sessions.notify[s.AgentID]; signal != nil {
		close(signal)
		delete(sessions.notify, s.AgentID)
	}
	sessions.Unlock()
}

func (s *Session) EnqueueCommand(sequence uint64, command Command) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed != nil {
		return s.commandSeq, ErrClosed
	}
	if sequence == 0 || sequence > s.commandSeq+1 {
		return s.commandSeq, ErrSequence
	}
	if sequence <= s.commandSeq {
		return s.commandSeq, nil
	}
	if len(s.commands) >= maxBufferedCommands {
		return s.commandSeq, ErrCommandBacklog
	}
	s.commandSeq = sequence
	s.commands = append(s.commands, command.Response(sequence))
	s.signalCommands()
	return s.commandSeq, nil
}

func (s *Session) NextCommand(ctx context.Context, after uint64) (*websshv1.AttachSessionResponse, error) {
	for {
		s.mu.Lock()
		for _, command := range s.commands {
			if command.Sequence > after {
				result := command
				s.mu.Unlock()
				return result, nil
			}
		}
		if s.closed != nil {
			closed := &websshv1.AttachSessionResponse{Sequence: s.commandSeq + 1, Command: &websshv1.AttachSessionResponse_CloseReason{CloseReason: s.closed.Reason.String()}}
			s.mu.Unlock()
			return closed, nil
		}
		signal := s.commandNotify
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-signal:
		}
	}
}

func (s *Session) AppendAgentEvent(event *websshv1.AgentSessionEvent) (uint64, error) {
	if event == nil || (event.Sequence == 0 && event.AcceptedCommandSequence == 0) {
		return 0, ErrSequence
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if event.AcceptedCommandSequence > s.commandSeq {
		return s.agentSeq, ErrSequence
	}
	if event.AcceptedCommandSequence > 0 {
		commands := s.commands[:0]
		for _, command := range s.commands {
			if command.Sequence > event.AcceptedCommandSequence {
				commands = append(commands, command)
			}
		}
		s.commands = commands
	}
	if event.Sequence == 0 && event.Event == nil {
		return s.agentSeq, nil
	}
	if event.Sequence <= s.agentSeq {
		return s.agentSeq, nil
	}
	if event.Sequence != s.agentSeq+1 {
		return s.agentSeq, ErrSequence
	}
	buffered := uint64(len(event.GetOutput()))
	if file := event.GetFile(); file != nil {
		buffered += uint64(len(file.Data))
	}
	if buffered+s.bufferedOutput > maxBufferedOutput {
		return s.agentSeq, ErrOutputLimit
	}
	s.bufferedOutput += buffered
	occurredAt := event.OccurredAt
	if occurredAt == nil || !occurredAt.IsValid() {
		occurredAt = timestamppb.Now()
	}
	stored := &websshv1.SessionEvent{SessionId: s.ID, Sequence: event.Sequence, OccurredAt: occurredAt}
	switch value := event.Event.(type) {
	case *websshv1.AgentSessionEvent_Output:
		stored.Event = &websshv1.SessionEvent_Output{Output: append([]byte(nil), value.Output...)}
	case *websshv1.AgentSessionEvent_File:
		stored.Event = &websshv1.SessionEvent_File{File: value.File}
	case *websshv1.AgentSessionEvent_Closed:
		stored.Event = &websshv1.SessionEvent_Closed{Closed: value.Closed}
		s.closed = value.Closed
	default:
		return s.agentSeq, errors.New("agent session event is empty")
	}
	s.agentSeq = event.Sequence
	s.events = append(s.events, stored)
	s.signalEvents()
	return s.agentSeq, nil
}

func (s *Session) AcknowledgeEvents(sequence uint64) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sequence > s.agentSeq {
		return s.agentSeq, ErrSequence
	}
	kept := s.events[:0]
	for _, event := range s.events {
		if event.Sequence > sequence {
			kept = append(kept, event)
			continue
		}
		released := uint64(len(event.GetOutput()))
		if file := event.GetFile(); file != nil {
			released += uint64(len(file.Data))
		}
		if released >= s.bufferedOutput {
			s.bufferedOutput = 0
		} else {
			s.bufferedOutput -= released
		}
	}
	s.events = kept
	return sequence, nil
}

func (s *Session) NextEvent(ctx context.Context, after uint64) (*websshv1.SessionEvent, error) {
	for {
		s.mu.Lock()
		for _, event := range s.events {
			if event.Sequence > after {
				s.mu.Unlock()
				return event, nil
			}
		}
		if s.closed != nil {
			s.mu.Unlock()
			return nil, ErrClosed
		}
		signal := s.eventNotify
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-signal:
		}
	}
}

func (s *Session) Close(reason websshv1.CloseReason) *websshv1.SessionClosed {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed == nil {
		s.closed = &websshv1.SessionClosed{SessionId: s.ID, Reason: reason, ClosedAt: timestamppb.Now()}
		s.signalCommands()
		s.signalEvents()
	}
	return s.closed
}

type Command interface {
	Response(uint64) *websshv1.AttachSessionResponse
}

type Input []byte

func (input Input) Response(sequence uint64) *websshv1.AttachSessionResponse {
	return &websshv1.AttachSessionResponse{Sequence: sequence, Command: &websshv1.AttachSessionResponse_Input{Input: append([]byte(nil), input...)}}
}

type Resize struct{ Size *websshv1.TerminalSize }

func (resize Resize) Response(sequence uint64) *websshv1.AttachSessionResponse {
	return &websshv1.AttachSessionResponse{Sequence: sequence, Command: &websshv1.AttachSessionResponse_Resize{Resize: cloneSize(resize.Size)}}
}

type File struct{ Command *websshv1.FileCommand }

func (file File) Response(sequence uint64) *websshv1.AttachSessionResponse {
	return &websshv1.AttachSessionResponse{Sequence: sequence, Command: &websshv1.AttachSessionResponse_File{File: file.Command}}
}

func (s *Session) assignment() *websshv1.SessionAssignment {
	return &websshv1.SessionAssignment{AssignmentId: s.AssignmentID, SessionId: s.ID, AgentId: s.AgentID, Shell: s.Shell,
		Size: cloneSize(s.Size), WorkingDirectory: s.WorkingDirectory, LeaseExpiresAt: timestamppb.New(s.LeaseExpiresAt)}
}

func cloneSize(size *websshv1.TerminalSize) *websshv1.TerminalSize {
	if size == nil {
		return nil
	}
	return &websshv1.TerminalSize{Rows: size.Rows, Columns: size.Columns}
}

func (s *Session) signalCommands() {
	close(s.commandNotify)
	s.commandNotify = make(chan struct{})
}

func (s *Session) signalEvents() {
	close(s.eventNotify)
	s.eventNotify = make(chan struct{})
}

func pruneLocked(now time.Time) {
	for id, session := range sessions.values {
		session.mu.Lock()
		expired := now.Sub(session.CreatedAt) > maxSessionDuration
		closed := session.closed != nil && session.closed.ClosedAt != nil && session.closed.ClosedAt.IsValid() && now.Sub(session.closed.ClosedAt.AsTime()) > closedRetention
		session.mu.Unlock()
		if expired || closed {
			delete(sessions.values, id)
		}
	}
}

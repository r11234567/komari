package agentevents

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	agentv1 "github.com/r11234567/komari-proto/gen/go/komari/agent/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maximumEventsPerAgent = 128
	maximumMessageBytes   = 1024
	maximumMetadata       = 32
)

var ErrInvalidEvent = errors.New("invalid agent event")

var store = struct {
	sync.Mutex
	agentEvents  map[string]map[string]*agentv1.AgentEvent
	serverEvents map[string][]*agentv1.ServerEvent
	acknowledged map[string]string
	notify       map[string]chan struct{}
}{
	agentEvents:  make(map[string]map[string]*agentv1.AgentEvent),
	serverEvents: make(map[string][]*agentv1.ServerEvent),
	acknowledged: make(map[string]string),
	notify:       make(map[string]chan struct{}),
}

func Publish(agentID string, event *agentv1.AgentEvent) (string, error) {
	if event == nil || strings.TrimSpace(event.EventId) == "" || event.Type == agentv1.AgentEventType_AGENT_EVENT_TYPE_UNSPECIFIED {
		return "", ErrInvalidEvent
	}
	if strings.TrimSpace(event.AgentId) != "" && event.AgentId != agentID {
		return "", ErrInvalidEvent
	}
	if len(event.Message) > maximumMessageBytes || len(event.Metadata) > maximumMetadata {
		return "", ErrInvalidEvent
	}
	store.Lock()
	defer store.Unlock()
	byID := store.agentEvents[agentID]
	if byID == nil {
		byID = make(map[string]*agentv1.AgentEvent)
		store.agentEvents[agentID] = byID
	}
	if _, exists := byID[event.EventId]; exists {
		return event.EventId, nil
	}
	copy := proto.Clone(event).(*agentv1.AgentEvent)
	copy.AgentId = agentID
	if copy.OccurredAt == nil || !copy.OccurredAt.IsValid() {
		copy.OccurredAt = timestamppb.Now()
	}
	if len(byID) >= maximumEventsPerAgent {
		for id := range byID {
			delete(byID, id)
			break
		}
	}
	byID[copy.EventId] = copy
	return copy.EventId, nil
}

func Notify(agentID string, eventType agentv1.ServerEventType, metadata map[string]string) {
	if agentID == "" || eventType == agentv1.ServerEventType_SERVER_EVENT_TYPE_UNSPECIFIED {
		return
	}
	store.Lock()
	defer store.Unlock()
	event := &agentv1.ServerEvent{EventId: uuid.NewString(), CreatedAt: timestamppb.New(time.Now().UTC()), Type: eventType, Metadata: make(map[string]string, len(metadata))}
	for key, value := range metadata {
		if len(event.Metadata) >= maximumMetadata {
			break
		}
		if key = strings.TrimSpace(key); key != "" && len(key) <= 128 && len(value) <= maximumMessageBytes {
			event.Metadata[key] = value
		}
	}
	events := append(store.serverEvents[agentID], event)
	if len(events) > maximumEventsPerAgent {
		events = events[len(events)-maximumEventsPerAgent:]
	}
	store.serverEvents[agentID] = events
	if signal := store.notify[agentID]; signal != nil {
		close(signal)
		delete(store.notify, agentID)
	}
}

func Next(ctx context.Context, agentID, afterID string) (*agentv1.ServerEvent, error) {
	for {
		store.Lock()
		events := store.serverEvents[agentID]
		index := -1
		if afterID != "" {
			for i, event := range events {
				if event.EventId == afterID {
					index = i
					break
				}
			}
		}
		if len(events) > 0 && index+1 < len(events) {
			event := proto.Clone(events[index+1]).(*agentv1.ServerEvent)
			store.Unlock()
			return event, nil
		}
		signal := store.notify[agentID]
		if signal == nil {
			signal = make(chan struct{})
			store.notify[agentID] = signal
		}
		store.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-signal:
		}
	}
}

func Acknowledge(agentID, eventID string) bool {
	if agentID == "" || eventID == "" {
		return false
	}
	store.Lock()
	defer store.Unlock()
	for _, event := range store.serverEvents[agentID] {
		if event.EventId == eventID {
			store.acknowledged[agentID] = eventID
			return true
		}
	}
	return false
}

package agentcredentials

import (
	"context"
	"sync"
)

type activeStream struct{ cancel context.CancelFunc }

var activeStreams = struct {
	sync.Mutex
	byAgent map[string]map[*activeStream]struct{}
}{byAgent: make(map[string]map[*activeStream]struct{})}

func TrackStream(ctx context.Context, agentID string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stream := &activeStream{cancel: cancel}
	activeStreams.Lock()
	if activeStreams.byAgent[agentID] == nil {
		activeStreams.byAgent[agentID] = make(map[*activeStream]struct{})
	}
	activeStreams.byAgent[agentID][stream] = struct{}{}
	activeStreams.Unlock()
	return ctx, func() {
		activeStreams.Lock()
		delete(activeStreams.byAgent[agentID], stream)
		if len(activeStreams.byAgent[agentID]) == 0 {
			delete(activeStreams.byAgent, agentID)
		}
		activeStreams.Unlock()
		cancel()
	}
}

func CancelStreams(agentID string) {
	activeStreams.Lock()
	defer activeStreams.Unlock()
	for stream := range activeStreams.byAgent[agentID] {
		stream.cancel()
	}
	delete(activeStreams.byAgent, agentID)
}

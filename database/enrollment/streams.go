package enrollment

import (
	"context"
	"sync"
)

type activeStream struct{ cancel context.CancelFunc }

var activeStreams = struct {
	sync.Mutex
	byAgent map[string]map[*activeStream]struct{}
}{byAgent: make(map[string]map[*activeStream]struct{})}

// TrackStream registers before checking the token. A revocation committed
// before registration is seen by the check, and a later one cancels the
// registered stream, including while the check is in progress.
func TrackStream(ctx context.Context, agentID, token string) (context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(ctx)
	stream := &activeStream{cancel: cancel}
	activeStreams.Lock()
	if activeStreams.byAgent[agentID] == nil {
		activeStreams.byAgent[agentID] = make(map[*activeStream]struct{})
	}
	activeStreams.byAgent[agentID][stream] = struct{}{}
	activeStreams.Unlock()
	stop := func() {
		activeStreams.Lock()
		delete(activeStreams.byAgent[agentID], stream)
		if len(activeStreams.byAgent[agentID]) == 0 {
			delete(activeStreams.byAgent, agentID)
		}
		activeStreams.Unlock()
		cancel()
	}
	id, _, err := LookupAccessTokenContext(ctx, token)
	if err == nil && id != agentID {
		err = ErrCredentialRejected
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		stop()
		return nil, nil, err
	}
	return ctx, stop, nil
}

func cancelStreams(agentID string) {
	activeStreams.Lock()
	defer activeStreams.Unlock()
	for stream := range activeStreams.byAgent[agentID] {
		stream.cancel()
	}
	delete(activeStreams.byAgent, agentID)
}

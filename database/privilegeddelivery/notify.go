package privilegeddelivery

import "sync"

// Notifications are hints to re-read committed state, never authorization.
// Subscribers register before reading, so a commit between read and wait is
// not lost. Cross-process changes are covered by the bounded watch heartbeat.
var watchers = struct {
	sync.Mutex
	agents map[string]map[chan struct{}]struct{}
}{agents: make(map[string]map[chan struct{}]struct{})}

func Subscribe(id string) (<-chan struct{}, func()) {
	ch := make(chan struct{})
	watchers.Lock()
	if watchers.agents[id] == nil {
		watchers.agents[id] = make(map[chan struct{}]struct{})
	}
	watchers.agents[id][ch] = struct{}{}
	watchers.Unlock()
	return ch, func() {
		watchers.Lock()
		defer watchers.Unlock()
		delete(watchers.agents[id], ch)
		if len(watchers.agents[id]) == 0 {
			delete(watchers.agents, id)
		}
	}
}
func Notify(id string) {
	watchers.Lock()
	defer watchers.Unlock()
	for ch := range watchers.agents[id] {
		close(ch)
	}
	delete(watchers.agents, id)
}

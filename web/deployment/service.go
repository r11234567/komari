// Package deployment is the canonical application service for desired Agent
// configuration. Connect is the primary transport; v2 JSON-RPC is only used
// to wake already-connected legacy Agents while they upgrade.
package deployment

import (
	"context"
	"errors"
	"sync"

	"github.com/komari-monitor/komari/database/clients"
	agent_runtime "github.com/komari-monitor/komari/web/agent"
)

type SaveResult struct {
	Profile        clients.DeploymentProfile
	Delivery       clients.DeploymentDeliveryState
	RuntimeChanged bool
}

var configSignals = struct {
	sync.Mutex
	channels map[string]chan struct{}
	watches  map[string]int
}{channels: make(map[string]chan struct{}), watches: make(map[string]int)}

func signal(agentID string) {
	configSignals.Lock()
	current := configSignals.channels[agentID]
	if current != nil {
		close(current)
	}
	configSignals.channels[agentID] = make(chan struct{})
	configSignals.Unlock()
}

func signalFor(agentID string) chan struct{} {
	configSignals.Lock()
	defer configSignals.Unlock()
	current := configSignals.channels[agentID]
	if current == nil {
		current = make(chan struct{})
		configSignals.channels[agentID] = current
	}
	return current
}

func registerWatch(agentID string) func() {
	configSignals.Lock()
	configSignals.watches[agentID]++
	configSignals.Unlock()
	return func() {
		configSignals.Lock()
		configSignals.watches[agentID]--
		if configSignals.watches[agentID] <= 0 {
			delete(configSignals.watches, agentID)
		}
		configSignals.Unlock()
	}
}

func hasWatch(agentID string) bool {
	configSignals.Lock()
	defer configSignals.Unlock()
	return configSignals.watches[agentID] > 0
}

// Save stores a normalized profile and creates a fresh revision for an
// explicit redispatch. It wakes Connect consumers and adapts delivery to v2
// only for legacy Agents that are already connected.
func Save(agentID string, profile clients.DeploymentProfile, forceDispatch bool) (SaveResult, error) {
	// A forced redispatch only applies to the existing online runtime contract.
	// Saving install-only changes still updates the profile but cannot fabricate
	// an online delivery revision for a running Agent.
	stored, delivery, changed, err := clients.SaveDeploymentProfileForDispatchForced(agentID, profile, forceDispatch)
	if err != nil {
		return SaveResult{}, err
	}
	result := SaveResult{Profile: stored, Delivery: delivery, RuntimeChanged: changed}
	if !changed {
		return result, nil
	}

	connectOnline := hasWatch(agentID)
	signal(agentID)
	if connectOnline {
		return result, nil
	}
	// Unary Connect Agents poll GetDesiredConfig. Keep the state saved until
	// their next poll marks the revision sent and their ACK closes delivery.
	if agent_runtime.IsConnectClient(agentID) && agent_runtime.IsAgentOnline(agentID) {
		return result, nil
	}
	legacyConfig := stored.RuntimeConfig()
	legacyConfig.Revision = delivery.Revision
	_, sent, supported := agent_runtime.DispatchV2Config(agentID, legacyConfig)
	if sent {
		_, err = clients.MarkDeploymentConfigSent(agentID, delivery.Revision)
		if err == nil {
			result.Delivery.Status = clients.DeploymentDeliverySent
		}
		return result, err
	}
	if !supported && !connectOnline {
		_, err = clients.MarkDeploymentConfigUnavailable(agentID, delivery.Revision, agent_runtime.IsAgentOnline(agentID))
		if err == nil {
			if agent_runtime.IsAgentOnline(agentID) {
				result.Delivery.Status = clients.DeploymentDeliveryUpgradeRequired
			} else {
				result.Delivery.Status = clients.DeploymentDeliveryOffline
			}
		}
	}
	return result, err
}

func RollbackOnlineConfig(agentID string) (SaveResult, error) {
	profile, available, err := clients.PreviousDeploymentRuntimeProfile(agentID)
	if err != nil {
		return SaveResult{}, err
	}
	if !available {
		return SaveResult{}, errors.New("no previous online configuration is available")
	}
	return Save(agentID, profile, true)
}

// GetDesired returns a newer desired snapshot, if one exists.
func GetDesired(agentID string, appliedRevision uint64) (clients.DeploymentProfile, bool, clients.DeploymentDeliveryState, error) {
	profile, saved, state, err := clients.GetDeploymentProfileWithDelivery(agentID)
	if err != nil || !saved || state.Revision <= appliedRevision {
		return profile, false, state, err
	}
	return profile, true, state, nil
}

// WaitForDesired blocks until a newer revision is persisted or cancellation
// reaches the caller. The returned snapshot is read again after every signal.
func WaitForDesired(ctx context.Context, agentID string, appliedRevision uint64) (clients.DeploymentProfile, bool, clients.DeploymentDeliveryState, error) {
	unregister := registerWatch(agentID)
	defer unregister()
	for {
		currentSignal := signalFor(agentID)
		profile, found, state, err := GetDesired(agentID, appliedRevision)
		if err != nil || found {
			return profile, found, state, err
		}
		select {
		case <-ctx.Done():
			return clients.DeploymentProfile{}, false, clients.DeploymentDeliveryState{}, ctx.Err()
		case <-currentSignal:
		}
	}
}

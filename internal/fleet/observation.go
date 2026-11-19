package fleet

import (
	"context"
	"sync"
	"time"

	"github.com/flemzord/t3fleet/pkg/provider"
)

// ObservedMachine adds provider telemetry to an inventory response. Only the
// embedded Machine is durable; neither an observation nor a failed read
// changes the worker's lifecycle or dispatch reservation.
type ObservedMachine struct {
	Machine
	ProviderState      string    `json:"providerState,omitempty"`
	ProviderObservedAt time.Time `json:"providerObservedAt,omitzero"`
}

// ObservedList takes one local snapshot, then reads each participating account
// independently. A slow account cannot hold the manager lock or hide other
// accounts' observations. Ordinary List remains local for command completion
// and machine resolution.
func (m *Manager) ObservedList(ctx context.Context) []ObservedMachine {
	machines := m.List()
	rows := make([]ObservedMachine, len(machines))
	groups := make(map[string][]int)
	for i, machine := range machines {
		rows[i].Machine = machine
		if _, ok := m.opts.Providers[machine.Provider].(provider.StateObserver); !ok {
			continue
		}
		rows[i].ProviderState = "unknown"
		if machine.RemoteID != "" {
			groups[machine.Provider] = append(groups[machine.Provider], i)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for name, indices := range groups {
		observer := m.opts.Providers[name].(provider.StateObserver)
		wg.Go(func() {
			ids := make([]string, len(indices))
			for j, i := range indices {
				ids[j] = rows[i].RemoteID
			}
			states, err := observer.ObserveStates(ctx, ids)
			if err != nil {
				return
			}
			at := m.opts.Now().UTC()
			for _, i := range indices {
				if state := states[rows[i].RemoteID]; state != "" {
					rows[i].ProviderState = state
					rows[i].ProviderObservedAt = at
				}
			}
		})
	}
	wg.Wait()
	return rows
}

package proxy

import (
	"context"
	"errors"
	"maps"
)

// enableWorkerAgents turns on each declared agent in the worker's T3
// settings. T3 leaves some drivers, such as Pi, disabled by default, and a
// disabled driver only offers its placeholder model. A settings patch
// replaces the whole providerInstances map, so the current map is read and
// merged first.
func (d *dispatcher) enableWorkerAgents(ctx context.Context, worker DispatchWorker) error {
	var current map[string]any
	found := false
	read := map[string]any{"_tag": "Request", "id": "agent-settings-read", "tag": "server.getConfig", "headers": []any{}, "payload": map[string]any{}}
	if err := d.rpcTo(ctx, worker, read, nil, func(reply map[string]any) error {
		exit, _ := reply["exit"].(map[string]any)
		value, _ := exit["value"].(map[string]any)
		if settings, ok := value["settings"].(map[string]any); ok && exit["_tag"] == "Success" {
			current, _ = settings["providerInstances"].(map[string]any)
			found = true
		}
		return nil
	}); err != nil || !found {
		return errors.New("could not read the worker's agent settings")
	}
	instances, changed := enabledAgentInstances(current, d.onDemand.agents())
	if !changed {
		return nil
	}
	ok := false
	update := map[string]any{"_tag": "Request", "id": "agent-settings", "tag": "server.updateSettings", "headers": []any{},
		"payload": map[string]any{"patch": map[string]any{"providerInstances": instances}}}
	if err := d.rpcTo(ctx, worker, update, nil, func(reply map[string]any) error {
		if exit, yes := reply["exit"].(map[string]any); yes && exit["_tag"] == "Success" {
			ok = true
		}
		return nil
	}); err != nil || !ok {
		return errors.New("could not enable the declared agents on the worker")
	}
	return nil
}

// enabledAgentInstances returns the provider instance map with each declared
// driver enabled under its default instance ID. Other entries and fields are
// kept as they are.
func enabledAgentInstances(current map[string]any, agents []DispatchAgent) (map[string]any, bool) {
	instances := maps.Clone(current)
	if instances == nil {
		instances = map[string]any{}
	}
	changed := false
	for _, agent := range agents {
		instance, _ := instances[agent.Driver].(map[string]any)
		if instance == nil || instance["driver"] != agent.Driver {
			instance = map[string]any{"driver": agent.Driver}
		} else if instance["enabled"] == true {
			continue
		} else {
			instance = maps.Clone(instance)
		}
		instance["enabled"] = true
		instances[agent.Driver] = instance
		changed = true
	}
	return instances, changed
}

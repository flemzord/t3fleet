package proxy

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"
)

// Keep the last native listing so Desktop can open a stopped worker. The
// index contains navigation metadata, never messages, prompts or credentials.
type threadListing struct {
	Schema   float64
	Sequence float64
	Archived bool
	Thread   map[string]any
}

func findThreadListing(snapshot map[string]any, id, projectID string) *threadListing {
	schema, ok := snapshot["schemaVersion"].(float64)
	if !ok || schema < 1 {
		return nil
	}
	sequence, _ := snapshot["snapshotSequence"].(float64)
	for _, group := range []string{"threads", "archivedThreads"} {
		items, _ := snapshot[group].([]any)
		for _, item := range items {
			row, _ := item.(map[string]any)
			if row["id"] == id && row["projectId"] == projectID {
				return &threadListing{Schema: schema, Sequence: sequence, Archived: group == "archivedThreads", Thread: row}
			}
		}
	}
	return nil
}

// Copy only the fields needed for a valid native shell row and navigation.
// Live rows still include the complete native response.
func (l threadListing) navigationOnly() *threadListing {
	row := make(map[string]any)
	for _, key := range []string{"id", "projectId", "title", "createdBy", "creationSource", "providerInstanceId",
		"modelSelection", "runtimeMode", "interactionMode", "branch", "worktreePath", "lineage", "forkedFrom", "linkOrigin",
		"activeProviderThreadId", "latestRunId", "activeRunId", "status", "latestUserMessageAt", "hasActionableProposedPlan",
		"itemCount", "visibleItemCount", "createdAt", "updatedAt", "archivedAt", "settledOverride", "settledAt",
		"pinnedAt", "pinOrderKey", "activeOrderKey", "deletedAt"} {
		if v, ok := l.Thread[key]; ok {
			row[key] = v
		}
	}
	row["latestVisibleMessage"], row["pendingRuntimeRequest"] = nil, nil
	l.Thread = row
	return &l
}

// Refresh using the connection already opened for an explicit thread action.
// No cloud adapter or power operation is called. A confirmed launch needs a
// durable native row before we acknowledge it; replay uses the same worker.
func (d *dispatcher) observeThreadListing(ctx context.Context, up Upstream, routeID, id string, launch bool) error {
	route := d.demandRoute(routeID)
	if !route.Ready || id == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	snapshot, err := fetchSnapshot(ctx, up, dispatchShellPath)
	if err == nil {
		if entry := findThreadListing(snapshot, id, route.ProjectID); entry != nil {
			err = d.saveThreadListing(routeID, route.Key, entry)
			if err == nil {
				return nil
			}
		}
	}
	if launch && route.Listing == nil {
		return errors.New("the thread started but its listing could not be confirmed; retry the same launch")
	}
	return nil
}

func (d *dispatcher) observeThreadRPC(ctx context.Context, up Upstream, frame, reply map[string]any) error {
	exit, _ := reply["exit"].(map[string]any)
	tag, _ := frame["tag"].(string)
	if exit["_tag"] != "Success" || (tag != "orchestration.launchThread" && tag != "orchestration.getThreadProjection" && tag != "orchestration.dispatchCommand") {
		return nil
	}
	payload, _ := frame["payload"].(map[string]any)
	id, _ := payload["threadId"].(string)
	return d.observeThreadListing(ctx, up, id, id, tag == "orchestration.launchThread")
}

func (d *dispatcher) saveThreadListing(id, key string, listing *threadListing) error {
	next := listing.navigationOnly()
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.disk.OnDemand.Routes[id]
	if !ok || r.Key != key || r.Lost != nil || r.Archive != nil {
		return nil
	}
	if old := r.Listing; old != nil && (old.Sequence > next.Sequence || old.Schema == next.Schema &&
		old.Archived == next.Archived && reflect.DeepEqual(old.Thread, next.Thread)) {
		return nil
	}
	r.Listing = next
	return d.saveDemandRouteLocked(id, r)
}

// RestoreWorkerListing recovers missing navigation metadata after the operator
// explicitly starts a worker. Looking up route ownership uses only local
// state; only the selected worker's T3 endpoint can be opened here.
func (p *Proxy) RestoreWorkerListing(ctx context.Context, machineID string) error {
	d := p.dispatch
	d.mu.Lock()
	routes := maps.Clone(d.disk.OnDemand.Routes)
	d.mu.Unlock()
	for id, r := range routes {
		if !r.Ready || r.Listing != nil || r.Archive != nil || r.Lost != nil {
			continue
		}
		worker, err := d.onDemand.Lookup(r.Key)
		if err != nil || worker.ID != machineID {
			continue
		}
		snapshot, err := d.getFrom(ctx, worker, dispatchShellPath)
		if err != nil {
			return err
		}
		row := findThreadListing(snapshot, id, r.ProjectID)
		if row == nil {
			return errors.New("reserved thread is absent from its worker's shell")
		}
		return d.saveThreadListing(id, r.Key, row)
	}
	return nil
}

// The controller owns all projects. One unreachable worker must neither wake
// up nor suppress the rest of the fleet, including the thread used to wake it.
func (d *dispatcher) onDemandShell(ctx context.Context) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	controller, err := d.getFrom(ctx, d.onDemand.Controller, dispatchShellPath)
	if err != nil {
		return nil, err
	}
	schema, ok := controller["schemaVersion"].(float64)
	if !ok {
		return nil, errors.New("invalid controller shell")
	}
	if projects, ok := controller["projects"].([]any); ok {
		for _, item := range projects {
			if project, ok := item.(map[string]any); ok {
				d.decorateProject(project)
			}
		}
	}

	d.mu.Lock()
	saved := maps.Clone(d.disk.OnDemand.Routes)
	d.mu.Unlock()
	ids := slices.Sorted(maps.Keys(saved))
	listings := make([]*threadListing, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		route := saved[id]
		if !route.Ready || route.Archive != nil || route.Lost != nil {
			continue
		}
		if cached := route.Listing; cached != nil && cached.Schema == schema && cached.Thread["id"] == id {
			listings[i] = cached
		}
		wg.Go(func() {
			worker, err := d.onDemand.Lookup(route.Key)
			if err != nil || worker.ResolvePassive == nil {
				return
			}
			passiveCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			defer stop()
			worker.Resolve = worker.ResolvePassive
			snapshot, err := d.getFrom(passiveCtx, worker, dispatchShellPath)
			if err != nil || snapshot["schemaVersion"] != schema {
				return
			}
			if live := findThreadListing(snapshot, id, route.ProjectID); live != nil {
				listings[i] = live
				// A failed cache refresh must not hide a live worker or a sibling.
				_ = d.saveThreadListing(id, route.Key, live)
			}
		})
	}
	wg.Wait()
	threads, archived := []any{}, []any{}
	for _, l := range listings {
		if l != nil {
			if l.Archived {
				archived = append(archived, l.Thread)
			} else {
				threads = append(threads, l.Thread)
			}
		}
	}
	projects, ok := controller["projects"].([]any)
	if !ok {
		return nil, errors.New("invalid controller project catalog")
	}
	return map[string]any{"schemaVersion": schema, "snapshotSequence": 0, "projects": projects, "threads": threads, "archivedThreads": archived}, nil
}

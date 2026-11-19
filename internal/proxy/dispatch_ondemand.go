package proxy

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/flemzord/t3fleet/internal/state"
)

// DispatchOnDemand uses a local T3 server for the control plane. The callback
// is only invoked by a first message, with a stable allocation key.
type DispatchOnDemand struct {
	Controller  DispatchWorker
	ProjectRoot string
	MaxWorkers  int
	Model       string
	Provision   func(context.Context, string, string) (DispatchWorker, string, error)
	Lookup      func(string) (DispatchWorker, error)
	Release     func(context.Context, string) error // nil keeps archived workers
	Reclaim     func(context.Context, string) error // releases an incomplete launch, including one without a fleet batch
	// Lost retires the local worker record and reports true only when the
	// provider confirms absence. It does not delete remote machines.
	Lost func(context.Context, string) (bool, error)
}

type demandRoute struct {
	Key        string
	ProjectID  string
	Listing    *threadListing `json:",omitempty"`
	Ready      bool
	Peer       *peerRoute         `json:",omitempty"`
	Archive    *demandArchive     `json:",omitempty"`
	RetryUntil time.Time          `json:",omitzero"`
	Abandoned  *demandAbandonment `json:",omitempty"`
	Lost       *demandLoss        `json:",omitempty"`
}

type onDemandDisk struct {
	Projects    map[string]demandProject
	Project     *demandProject `json:",omitempty"` // Single-project state written by earlier versions.
	Routes      map[string]demandRoute
	PeerThreads map[string]string `json:",omitempty"`
}

func NewDispatchOnDemand(origin string, store *state.Store, opts DispatchOnDemand) (*Proxy, error) {
	if store == nil || opts.Controller.Resolve == nil || opts.Provision == nil || opts.Lookup == nil ||
		!filepath.IsAbs(opts.ProjectRoot) {
		return nil, errors.New("on-demand dispatch requires a local T3 controller, private project root and worker provisioner")
	}
	if opts.MaxWorkers < 1 {
		return nil, errors.New("on-demand dispatch requires a positive worker capacity")
	}
	d := &dispatcher{store: store, timeout: 3 * time.Minute,
		onDemand: &opts, preparing: map[string]chan struct{}{}}
	if err := store.Load(&d.disk); errors.Is(err, state.ErrEmpty) {
		id, err := randomToken()
		if err != nil {
			return nil, err
		}
		d.disk = dispatchState{Origin: origin, EnvironmentID: "t3fleet-" + id, Clients: map[string]time.Time{},
			OnDemand: &onDemandDisk{Projects: map[string]demandProject{}, Routes: map[string]demandRoute{}}}
		if err := store.Save(d.disk); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if d.disk.Origin != origin || d.disk.EnvironmentID == "" || d.disk.Clients == nil ||
		d.disk.OnDemand == nil || d.disk.OnDemand.Routes == nil {
		return nil, errors.New("on-demand dispatch state must use its original origin and mode")
	}
	if err := d.loadDemandProjects(); err != nil {
		return nil, err
	}
	return d.proxy(origin)
}

func demandKey(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func (d *dispatcher) threadWorker(threadID string) (DispatchWorker, error) {
	d.mu.Lock()
	r, ok := d.disk.OnDemand.Routes[threadID]
	d.mu.Unlock()
	if r.Lost != nil {
		return DispatchWorker{}, errWorkerLost
	}
	if !ok || !r.Ready || r.Archive != nil {
		return DispatchWorker{}, errors.New("thread has no ready dispatch worker")
	}
	return d.onDemand.Lookup(r.Key)
}

func (d *dispatcher) prepareDemandWorker(ctx context.Context, id, projectID string) (DispatchWorker, error) {
	for {
		d.mu.Lock()
		project := d.disk.OnDemand.Projects[projectID]
		if project.Title == "" || !validDispatchThreadID(id) {
			d.mu.Unlock()
			return DispatchWorker{}, errors.New("register the GitHub project before launching a thread")
		}
		route, exists := d.disk.OnDemand.Routes[id]
		if exists && route.ProjectID != projectID {
			d.mu.Unlock()
			return DispatchWorker{}, errors.New("a thread must keep its original project")
		}
		if route.Abandoned != nil {
			d.mu.Unlock()
			return DispatchWorker{}, errors.New("the incomplete dispatch launch expired; start a new thread")
		}
		if route.Archive != nil {
			d.mu.Unlock()
			return DispatchWorker{}, errors.New("the dispatch thread was archived")
		}
		if route.Lost != nil {
			d.mu.Unlock()
			return DispatchWorker{}, errWorkerLost
		}
		if exists && route.Ready {
			d.mu.Unlock()
			return d.onDemand.Lookup(route.Key)
		}
		if flight := d.preparing[id]; flight != nil {
			d.mu.Unlock()
			select {
			case <-ctx.Done():
				return DispatchWorker{}, ctx.Err()
			case <-flight:
				continue
			}
		}
		if !exists {
			used := 0
			for _, reserved := range d.disk.OnDemand.Routes {
				if (reserved.Archive == nil || !reserved.Archive.Released) &&
					(reserved.Abandoned == nil || !reserved.Abandoned.Released) &&
					(reserved.Lost == nil || !reserved.Lost.Released) {
					used++
				}
			}
			if used >= d.onDemand.MaxWorkers {
				d.mu.Unlock()
				return DispatchWorker{}, errors.New("the dispatch worker limit has been reached")
			}
			route = demandRoute{Key: demandKey(d.disk.EnvironmentID, "thread", id), ProjectID: projectID}
		}
		if d.onDemand.Reclaim != nil {
			route.RetryUntil = time.Now().UTC().Add(dispatchLaunchGrace)
		}
		if !exists || d.onDemand.Reclaim != nil {
			if err := d.saveDemandRouteLocked(id, route); err != nil {
				d.mu.Unlock()
				return DispatchWorker{}, err
			}
		}
		flight := make(chan struct{})
		d.preparing[id] = flight
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			delete(d.preparing, id)
			close(flight)
			d.mu.Unlock()
		}()
		worker, path, err := d.onDemand.Provision(ctx, route.Key, project.Remote)
		if err != nil {
			return DispatchWorker{}, err
		}
		if _, err := d.seedProject(ctx, worker, project, path); err != nil {
			return DispatchWorker{}, err
		}
		d.mu.Lock()
		route.Ready = true
		route.RetryUntil = time.Time{}
		err = d.saveDemandRouteLocked(id, route)
		d.mu.Unlock()
		return worker, err
	}
}

func (d *dispatcher) saveDemandRouteLocked(id string, r demandRoute) error {
	next := d.disk
	demand := *next.OnDemand
	demand.Routes = maps.Clone(demand.Routes)
	demand.Routes[id] = r
	next.OnDemand = &demand
	if err := d.store.Save(next); err != nil {
		return errors.New("could not persist the dispatch thread assignment")
	}
	d.disk = next
	return nil
}

func rpcSuccess(id, value any) map[string]any {
	return map[string]any{"_tag": "Exit", "requestId": id, "exit": map[string]any{"_tag": "Success", "value": value}}
}

func (d *dispatcher) callOnDemand(ctx context.Context, tag string, frame, payload map[string]any, acks <-chan map[string]any, send func(map[string]any) error) error {
	id := frame["id"]
	threadID, _ := payload["threadId"].(string)
	switch tag {
	case "projectClone.start":
		value, err := d.registerRepository(ctx, payload)
		if err != nil {
			return err
		}
		return send(rpcSuccess(id, value))
	case "orchestration.launchThread":
		projectID, _ := payload["projectId"].(string)
		commandID, _ := payload["commandId"].(string)
		if threadID == "" && commandID != "" {
			d.mu.Lock()
			threadID = demandKey(d.disk.EnvironmentID, "launch", commandID)
			d.mu.Unlock()
			payload["threadId"] = threadID
		}
		unlock := d.lockDemandWrite(threadID)
		defer unlock()
		if payload["reuseExistingThread"] == true {
			if route := d.demandRoute(threadID); route.ProjectID == "" || route.ProjectID != projectID {
				return errors.New("a thread must keep its original project")
			}
			worker, err := d.threadWorker(threadID)
			if err != nil {
				return err
			}
			return d.rpcTo(ctx, worker, frame, acks, send)
		}
		initial, ok := payload["initialMessage"].(map[string]any)
		initialAttachments, _ := initial["attachments"].([]any)
		strategy, _ := payload["workspaceStrategy"].(map[string]any)
		if !ok || commandID == "" || len(initialAttachments) != 0 || (strategy["type"] != "root" && strategy["type"] != "worktree") {
			return errors.New("start a new dispatch thread with a text message in a new or root checkout")
		}
		worker, err := d.prepareDemandWorker(ctx, threadID, projectID)
		if err != nil {
			return err
		}
		return d.rpcTo(ctx, worker, frame, acks, send)
	case "orchestration.dispatchCommand":
		cmd, _ := payload["type"].(string)
		if !slices.Contains([]string{"message.dispatch", "thread.visit", "thread.mark-unread", "thread.metadata.update",
			"thread.model-selection.set", "thread.stop", "thread.archive", "thread.unarchive", "thread.settle", "thread.pin", "thread.unpin"}, cmd) {
			return errors.New("this command is outside on-demand dispatch")
		}
		unlock := d.lockDemandWrite(threadID)
		defer unlock()
		if cmd == "thread.archive" && d.handlesDemandArchive(threadID) {
			return d.archiveDemandRPC(ctx, threadID, frame, send)
		}
		worker, err := d.threadWorker(threadID)
		if err != nil {
			return err
		}
		return d.rpcTo(ctx, worker, frame, acks, send)
	case "orchestration.subscribeThread", "orchestration.getThreadProjection", "orchestration.subscribePromptSuggestion",
		"orchestration.subscribeCommandOutput", "orchestration.getTurnItem", "orchestration.getFullThreadDiff",
		"orchestration.getNativeChildWork", "orchestration.getThreadFamily":
		worker, err := d.threadWorker(threadID)
		if err != nil {
			return err
		}
		return d.rpcTo(ctx, worker, frame, acks, send)
	case "vcs.listRefs":
		// The checkout does not exist yet. An unset branch uses the Git remote's default.
		return send(rpcSuccess(id, map[string]any{"refs": []any{}, "isRepo": true, "hasPrimaryRemote": true, "nextCursor": nil, "totalCount": 0}))
	case "subscribeVcsBranch":
		if err := send(map[string]any{"_tag": "Chunk", "requestId": id,
			"values": []any{map[string]any{"refName": nil, "isRepo": true}}}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	case "server.getConfig", "server.probe", "subscribeServerConfig", "subscribeProjectClones", "subscribeAuthAccess":
		return d.rpcTo(ctx, d.onDemand.Controller, frame, acks, send)
	case "server.refreshProviders":
		return send(rpcSuccess(id, map[string]any{"providers": d.demandProviders()}))
	default:
		return errors.New("RPC outside on-demand dispatch")
	}
}

func (d *dispatcher) demandProviders() []any {
	model := cmp.Or(d.onDemand.Model, "gpt-5.4")
	return []any{map[string]any{"instanceId": "codex", "driver": "codex", "enabled": true, "installed": true,
		"version": nil, "status": "ready", "auth": map[string]any{"status": "unknown"},
		"checkedAt": time.Now().UTC().Format(time.RFC3339Nano), "models": []any{
			map[string]any{"slug": model, "name": model, "isCustom": model != "gpt-5.4", "isDefault": true, "capabilities": nil},
		}}}
}

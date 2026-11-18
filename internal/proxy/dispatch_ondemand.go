package proxy

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
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
}

type demandProject struct {
	ID     string
	Title  string
	Remote string
}

type demandRoute struct {
	Key        string
	ProjectID  string
	Ready      bool
	Peer       *peerRoute         `json:",omitempty"`
	Archive    *demandArchive     `json:",omitempty"`
	RetryUntil time.Time          `json:",omitzero"`
	Abandoned  *demandAbandonment `json:",omitempty"`
}

type onDemandDisk struct {
	Project     *demandProject
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
	d := &dispatchPOC{store: store, workers: []DispatchWorker{opts.Controller}, timeout: 3 * time.Minute,
		onDemand: &opts, preparing: map[string]chan struct{}{}}
	if err := store.Load(&d.disk); errors.Is(err, state.ErrEmpty) {
		id, err := randomToken()
		if err != nil {
			return nil, err
		}
		d.disk = dispatchState{Origin: origin, EnvironmentID: "t3fleet-" + id, Clients: map[string]time.Time{},
			OnDemand: &onDemandDisk{Routes: map[string]demandRoute{}}}
		if err := store.Save(d.disk); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if d.disk.Origin != origin || d.disk.EnvironmentID == "" || d.disk.Clients == nil ||
		d.disk.OnDemand == nil || d.disk.OnDemand.Routes == nil || len(d.disk.Workers) != 0 {
		return nil, errors.New("on-demand dispatch state must use its original origin and mode")
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

var repositoryPath = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9_.-]{1,100}$`)

func demandRemote(raw string) string {
	if strings.HasPrefix(raw, "git@github.com:") {
		path := strings.TrimPrefix(raw, "git@github.com:")
		if safeRepositoryPath(path) {
			return raw
		}
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host != "github.com" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" ||
		!safeRepositoryPath(strings.TrimPrefix(u.Path, "/")) {
		return ""
	}
	if u.Scheme == "https" && u.User == nil {
		return raw
	}
	if u.Scheme == "ssh" && u.User != nil && u.User.String() == "git" {
		return "git@github.com:" + strings.TrimPrefix(u.Path, "/")
	}
	return ""
}

func safeRepositoryPath(path string) bool {
	repo := strings.TrimSuffix(path, ".git")
	return repositoryPath.MatchString(path) && !strings.HasSuffix(repo, "/.") && !strings.HasSuffix(repo, "/..") && !strings.HasSuffix(repo, "/")
}

func (d *dispatchPOC) registerRepository(ctx context.Context, payload map[string]any) (map[string]any, error) {
	id, _ := payload["projectId"].(string)
	title, _ := payload["title"].(string)
	raw, _ := payload["remoteUrl"].(string)
	remote := demandRemote(raw)
	if !validDispatchThreadID(id) || strings.TrimSpace(title) == "" || len(title) > 256 || remote == "" {
		return nil, errors.New("choose a GitHub repository URL without embedded credentials")
	}
	d.mu.Lock()
	if existing := d.disk.OnDemand.Project; existing != nil {
		if existing.Remote != remote {
			d.mu.Unlock()
			return nil, errors.New("on-demand dispatch supports one GitHub repository")
		}
		id, title = existing.ID, existing.Title
	} else {
		next := d.disk
		demand := *next.OnDemand
		demand.Project = &demandProject{ID: id, Title: title, Remote: remote}
		next.OnDemand = &demand
		if err := d.store.Save(next); err != nil {
			d.mu.Unlock()
			return nil, errors.New("could not save the project")
		}
		d.disk = next
	}
	d.mu.Unlock()
	path := filepath.Join(d.onDemand.ProjectRoot, demandKey("project", id))
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, errors.New("could not prepare virtual project directory")
	}
	if err := d.seedProject(ctx, d.workers[0], id, title, path); err != nil {
		return nil, err
	}
	return map[string]any{"projectId": id, "cwd": path, "remoteUrl": remote, "repository": nil}, nil
}

func (d *dispatchPOC) seedProject(ctx context.Context, worker DispatchWorker, id, title, path string) error {
	req := map[string]any{"_tag": "Request", "id": "dispatch-project", "tag": "projects.mutate", "headers": []any{},
		"payload": map[string]any{"type": "project.create", "projectId": id, "title": title, "workspaceRoot": path,
			"commandId": "dispatch-project-" + demandKey(id)}}
	return d.rpcTo(ctx, worker, req, nil, func(reply map[string]any) error {
		exit, _ := reply["exit"].(map[string]any)
		if exit["_tag"] != "Success" {
			return errors.New("T3 could not register the dispatch project")
		}
		return nil
	})
}

func (d *dispatchPOC) threadWorker(threadID string) (DispatchWorker, error) {
	if d.onDemand == nil {
		index, err := d.worker(threadID, false)
		if err != nil {
			return DispatchWorker{}, err
		}
		return d.workers[index], nil
	}
	d.mu.Lock()
	r, ok := d.disk.OnDemand.Routes[threadID]
	d.mu.Unlock()
	if !ok || !r.Ready || r.Archive != nil {
		return DispatchWorker{}, errors.New("thread has no ready dispatch worker")
	}
	return d.onDemand.Lookup(r.Key)
}

func (d *dispatchPOC) prepareDemandWorker(ctx context.Context, id, projectID string) (DispatchWorker, error) {
	for {
		d.mu.Lock()
		project := d.disk.OnDemand.Project
		if project == nil || project.ID != projectID || !validDispatchThreadID(id) {
			d.mu.Unlock()
			return DispatchWorker{}, errors.New("register the GitHub project before launching a thread")
		}
		route, exists := d.disk.OnDemand.Routes[id]
		if route.Abandoned != nil {
			d.mu.Unlock()
			return DispatchWorker{}, errors.New("the incomplete dispatch launch expired; start a new thread")
		}
		if route.Archive != nil {
			d.mu.Unlock()
			return DispatchWorker{}, errors.New("the dispatch thread was archived")
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
					(reserved.Abandoned == nil || !reserved.Abandoned.Released) {
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
		if err := d.seedProject(ctx, worker, project.ID, project.Title, path); err != nil {
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

func (d *dispatchPOC) saveDemandRouteLocked(id string, r demandRoute) error {
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

func (d *dispatchPOC) onDemandShell(ctx context.Context) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	d.mu.Lock()
	saved := maps.Clone(d.disk.OnDemand.Routes)
	d.mu.Unlock()
	workers := []DispatchWorker{d.workers[0]}
	routes := map[string]int{}
	for _, id := range slices.Sorted(maps.Keys(saved)) {
		r := saved[id]
		if !r.Ready || (r.Archive != nil && r.Archive.Released) {
			continue
		}
		worker, err := d.onDemand.Lookup(r.Key)
		if err != nil {
			continue
		} // deleted workers keep their reservation but have no shell rows
		routes[id] = len(workers)
		workers = append(workers, worker)
	}
	rows := make([]map[string]any, len(workers))
	var wg sync.WaitGroup
	for i, worker := range workers {
		wg.Go(func() { rows[i], _ = d.getFrom(ctx, worker, dispatchShellPath) })
	}
	wg.Wait()
	// Tell Desktop to start a draft in the project root. This virtual
	// project has no checkout to inspect before the first message.
	if projects, ok := rows[0]["projects"].([]any); ok {
		for _, item := range projects {
			if project, ok := item.(map[string]any); ok {
				project["defaultThreadEnvMode"] = "local"
			}
		}
	}
	return mergeDispatchShell(rows, routes, true)
}

func rpcSuccess(id, value any) map[string]any {
	return map[string]any{"_tag": "Exit", "requestId": id, "exit": map[string]any{"_tag": "Success", "value": value}}
}

func (d *dispatchPOC) callOnDemand(ctx context.Context, tag string, frame, payload map[string]any, acks <-chan map[string]any, send func(map[string]any) error) error {
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
		return d.rpc(ctx, 0, frame, acks, send)
	case "server.refreshProviders":
		return send(rpcSuccess(id, map[string]any{"providers": d.demandProviders()}))
	default:
		return errors.New("RPC outside on-demand dispatch")
	}
}

func (d *dispatchPOC) demandProviders() []any {
	model := cmp.Or(d.onDemand.Model, "gpt-5.4")
	return []any{map[string]any{"instanceId": "codex", "driver": "codex", "enabled": true, "installed": true,
		"version": nil, "status": "ready", "auth": map[string]any{"status": "unknown"},
		"checkedAt": time.Now().UTC().Format(time.RFC3339Nano), "models": []any{
			map[string]any{"slug": model, "name": model, "isCustom": model != "gpt-5.4", "isDefault": true, "capabilities": nil},
		}}}
}

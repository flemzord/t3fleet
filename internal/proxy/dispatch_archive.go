package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sync"
	"time"
)

type demandArchive struct {
	Sequence uint64
	Released bool
}

func (d *dispatchPOC) demandWriteLock(id string) *sync.Mutex {
	h := sha256.Sum256([]byte(id))
	return &d.demandWrites[int(h[0])%len(d.demandWrites)]
}

func (d *dispatchPOC) lockDemandWrite(id string) func() {
	lock := d.demandWriteLock(id)
	lock.Lock()
	return lock.Unlock
}

func (d *dispatchPOC) handlesDemandArchive(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.onDemand.Release != nil || d.disk.OnDemand.Routes[id].Archive != nil
}

func (d *dispatchPOC) demandRoute(id string) demandRoute {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.disk.OnDemand.Routes[id]
}

func archiveSequence(value map[string]any) (uint64, bool) {
	// The websocket decoder preserves JSON numbers. The MCP decoder uses float64.
	switch n := value["sequence"].(type) {
	case json.Number:
		i, err := n.Int64()
		if err == nil && i >= 0 && i < 1<<53 {
			return uint64(i), true
		}
	case float64:
		if n >= 0 && n < 1<<53 && n == math.Trunc(n) {
			return uint64(n), true
		}
	}
	return 0, false
}

func alreadyArchivedMessage(id string) string { return "Thread " + id + " is already archived." }

// The pinned server rejects a new archive of an archived thread after its
// write authorization checks. Other native failures must never allow deletion
// on the strength of a successful read alone.
func desktopAlreadyArchived(exit map[string]any, id, commandID string) bool {
	causes, _ := exit["cause"].([]any)
	if commandID == "" || exit["_tag"] != "Failure" || len(causes) != 1 {
		return false
	}
	cause, _ := causes[0].(map[string]any)
	failure, _ := cause["error"].(map[string]any)
	return cause["_tag"] == "Fail" && failure["_tag"] == "OrchestrationV2DispatchCommandError" &&
		failure["commandId"] == commandID && failure["commandType"] == "thread.archive" && failure["detail"] == alreadyArchivedMessage(id)
}

func peerAlreadyArchived(reply map[string]any, id string) bool {
	result, _ := reply["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if result["isError"] != true || len(content) != 1 {
		return false
	}
	item, _ := content[0].(map[string]any)
	raw, _ := item["text"].(string)
	var failure struct {
		Tag     string `json:"_tag"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	return item["type"] == "text" && json.Unmarshal([]byte(raw), &failure) == nil &&
		failure.Tag == "OrchestratorMcpFailure" && failure.Code == "orchestration_error" &&
		failure.Message == alreadyArchivedMessage(id)
}

// releaseArchivedWorker is called with the thread's write lock held. Persist
// the native acknowledgement before deleting anything, and release capacity
// only once the provider and the dispatcher have both committed the deletion.
func (d *dispatchPOC) releaseArchivedWorker(ctx context.Context, id string, sequence uint64) (uint64, error) {
	d.mu.Lock()
	route, ok := d.disk.OnDemand.Routes[id]
	if !ok || !route.Ready {
		d.mu.Unlock()
		return 0, errors.New("the dispatch thread has no dedicated worker")
	}
	if route.Archive != nil {
		sequence = route.Archive.Sequence
		if route.Archive.Released {
			d.mu.Unlock()
			return sequence, nil
		}
	}
	if d.onDemand.Release == nil {
		d.mu.Unlock()
		return 0, errors.New("worker cleanup is pending; enable deleteWorkerOnArchive and retry archive")
	}
	if route.Archive == nil {
		route.Archive = &demandArchive{Sequence: sequence}
		if err := d.saveDemandRouteLocked(id, route); err != nil {
			d.mu.Unlock()
			return 0, errors.New("thread archived, but worker cleanup could not be saved; retry archive")
		}
	}
	d.mu.Unlock()

	cleanup, cancel := context.WithTimeout(ctx, d.timeout)
	err := d.onDemand.Release(cleanup, route.Key)
	cancel()
	if err != nil {
		return 0, errors.New("thread archived, but worker deletion is not confirmed; retry archive")
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	route.Archive = &demandArchive{Sequence: sequence, Released: true}
	if route.Peer != nil {
		// Retain the grant boundary for a replay, without retaining a
		// credential for the deleted worker.
		route.Peer = &peerRoute{Grant: route.Peer.Grant}
	}
	if err := d.saveDemandRouteLocked(id, route); err != nil {
		return 0, errors.New("worker deleted, but capacity release could not be saved; retry archive")
	}
	return sequence, nil
}

// RunDispatchCleanup retries archived deletes, reclaims incomplete launches
// and releases ready workers that their provider no longer has.
func (p *Proxy) RunDispatchCleanup(ctx context.Context, report func(error)) {
	d := p.dispatch
	if d == nil || d.onDemand == nil || (d.onDemand.Release == nil && d.onDemand.Reclaim == nil) {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var nextLostSweep time.Time
	for ctx.Err() == nil {
		if d.onDemand.Release != nil {
			d.retryDispatchArchives(ctx, report)
		}
		if d.onDemand.Reclaim != nil {
			d.reclaimDispatchLaunches(ctx, time.Now().UTC(), report)
		}
		if d.onDemand.Release != nil && d.onDemand.Lost != nil && !time.Now().Before(nextLostSweep) {
			d.releaseLostWorkers(ctx, report)
			nextLostSweep = time.Now().Add(lostWorkerSweepInterval)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *dispatchPOC) retryDispatchArchives(ctx context.Context, report func(error)) {
	d.mu.Lock()
	var ids []string
	for id, route := range d.disk.OnDemand.Routes {
		if route.Archive != nil && !route.Archive.Released {
			ids = append(ids, id)
		}
	}
	d.mu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		lock := d.demandWriteLock(id)
		if !lock.TryLock() {
			continue
		}
		_, err := d.releaseArchivedWorker(ctx, id, 0)
		lock.Unlock()
		if err != nil && ctx.Err() == nil {
			report(err)
		}
	}
}

func (d *dispatchPOC) archiveDemandRPC(ctx context.Context, id string, frame map[string]any, send func(map[string]any) error) error {
	var sequence uint64
	if d.demandRoute(id).Archive == nil {
		worker, err := d.threadWorker(id)
		if err != nil {
			return err
		}
		var reply map[string]any
		err = d.rpcTo(ctx, worker, frame, nil, func(result map[string]any) error {
			if result["_tag"] == "Exit" {
				reply = result
			}
			return nil
		})
		exit, _ := reply["exit"].(map[string]any)
		value, _ := exit["value"].(map[string]any)
		var confirmed bool
		if err == nil && exit["_tag"] == "Success" {
			sequence, confirmed = archiveSequence(value)
		}
		payload, _ := frame["payload"].(map[string]any)
		commandID, _ := payload["commandId"].(string)
		if !confirmed && !(err == nil && desktopAlreadyArchived(exit, id, commandID) && d.demandNativeArchived(ctx, worker, id)) {
			if err != nil {
				return err
			}
			if exit["_tag"] == "Failure" {
				return send(reply)
			}
			return errors.New("could not confirm the thread archive on the worker")
		}
	}
	sequence, err := d.releaseArchivedWorker(ctx, id, sequence)
	if err != nil {
		return err
	}
	return send(rpcSuccess(frame["id"], map[string]any{"sequence": sequence}))
}

// Recover an archive whose native reply was lost, or an explicit archive
// request for a thread archived before worker cleanup was enabled.
func (d *dispatchPOC) demandNativeArchived(ctx context.Context, worker DispatchWorker, id string) bool {
	frame := map[string]any{
		"_tag": "Request", "id": "dispatch-verify-archive", "tag": "orchestration.getThreadProjection",
		"headers": []any{}, "payload": map[string]any{"threadId": id},
	}
	confirmed := false
	err := d.rpcTo(ctx, worker, frame, nil, func(reply map[string]any) error {
		exit, _ := reply["exit"].(map[string]any)
		value, _ := exit["value"].(map[string]any)
		thread, _ := value["thread"].(map[string]any)
		archivedAt, _ := thread["archivedAt"].(string)
		confirmed = reply["_tag"] == "Exit" && exit["_tag"] == "Success" && thread["id"] == id && archivedAt != ""
		return nil
	})
	return err == nil && confirmed
}

func (d *dispatchPOC) archiveDemandPeer(w http.ResponseWriter, r *http.Request, request peerRequest, tool peerTool, id string, route demandRoute) {
	var sequence uint64
	if route.Archive == nil {
		worker, err := d.threadWorker(id)
		if err != nil {
			peerToolError(w, request.ID, "orchestration_error", "The linked worker is unavailable.")
			return
		}
		reply, err := d.sendPeerWorkerTool(r, worker, route.Peer.WorkerToken, request, tool)
		var confirmed bool
		if err == nil {
			sequence, confirmed = archiveSequence(peerStructuredResult(reply))
		}
		if !confirmed && err == nil && peerAlreadyArchived(reply, id) {
			read := peerTool{Name: "t3_thread_read", Arguments: map[string]any{"threadId": id, "limit": 1}}
			state, readErr := d.sendPeerWorkerTool(r, worker, route.Peer.WorkerToken, request, read)
			thread, _ := peerStructuredResult(state)["thread"].(map[string]any)
			confirmed = readErr == nil && thread["threadId"] == id && thread["archived"] == true
		}
		if !confirmed {
			if err == nil && peerStructuredResult(reply) == nil {
				d.writePeerResult(w, reply, tool.Name)
			} else {
				peerToolError(w, request.ID, "orchestration_error", "Could not confirm the thread archive on the linked worker.")
			}
			return
		}
	}
	sequence, err := d.releaseArchivedWorker(r.Context(), id, sequence)
	if err != nil {
		peerToolError(w, request.ID, "orchestration_error", err.Error())
		return
	}
	d.writePeerResult(w, map[string]any{
		"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": "Thread archived and its worker deleted."}},
			"structuredContent": map[string]any{"sequence": sequence},
		},
	}, tool.Name)
}

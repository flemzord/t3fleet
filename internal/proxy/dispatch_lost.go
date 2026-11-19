package proxy

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// lostWorkerSweepInterval limits provider status reads for ready workers.
// Each sweep reads every ready route once.
const lostWorkerSweepInterval = 10 * time.Minute

var errWorkerLost = errors.New("the dispatch worker no longer exists; start a new thread")

// A released loss is a durable tombstone for the thread. Retry any pending
// loss left by an interrupted cleanup before freeing its worker slot.
type demandLoss struct {
	Released bool
}

// A ready route normally keeps its capacity until its thread is archived. An
// archive needs the worker to confirm it, so a worker removed outside t3fleet
// would hold its slot forever.
func lostCandidate(route demandRoute) bool {
	return route.Ready && route.Archive == nil && (route.Lost == nil || !route.Lost.Released)
}

func (d *dispatcher) releaseLostWorkers(ctx context.Context, report func(error)) {
	d.mu.Lock()
	var ids []string
	for id, route := range d.disk.OnDemand.Routes {
		if lostCandidate(route) {
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
		err := d.releaseLostWorker(ctx, id)
		lock.Unlock()
		if err != nil && ctx.Err() == nil {
			report(fmt.Errorf("releasing lost dispatch worker %q: %w", id, err))
		}
	}
}

// The caller holds the thread's write lock.
func (d *dispatcher) releaseLostWorker(ctx context.Context, id string) error {
	d.mu.Lock()
	route, ok := d.disk.OnDemand.Routes[id]
	d.mu.Unlock()
	if !ok || !lostCandidate(route) {
		return nil
	}
	cleanup, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	// The fleet retires its local record before returning true and retains its
	// reservation tombstone. If saving the route fails, a retry sees that
	// tombstone without allocating, or deleting, a cloud machine.
	lost, err := d.onDemand.Lost(cleanup, route.Key)
	if err != nil || !lost {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	route.Lost = &demandLoss{Released: true}
	if route.Peer != nil {
		// Keep the grant boundary without a credential for the removed worker.
		route.Peer = &peerRoute{Grant: route.Peer.Grant}
	}
	return d.saveDemandRouteLocked(id, route)
}

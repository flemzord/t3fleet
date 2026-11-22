package proxy

import (
	"context"
	"fmt"
	"time"
)

const dispatchLaunchGrace = 30 * time.Minute

type demandAbandonment struct {
	Released bool
}

// Ready is persisted before a native launch is sent. A missing thread mapping
// alone cannot prove that a ready worker has no user work.
func reclaimableLaunch(route demandRoute) bool {
	return route.PoolSlot == 0 && !route.Ready && route.Peer == nil && route.Archive == nil &&
		(route.Abandoned == nil || !route.Abandoned.Released)
}

func (d *dispatcher) reclaimDispatchLaunches(ctx context.Context, now time.Time, report func(error)) {
	d.mu.Lock()
	var ids []string
	for id, route := range d.disk.OnDemand.Routes {
		if reclaimableLaunch(route) {
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
		err := d.reclaimDispatchLaunch(ctx, id, now)
		lock.Unlock()
		if err != nil && ctx.Err() == nil {
			report(fmt.Errorf("reclaiming incomplete dispatch launch %q: %w", id, err))
		}
	}
}

// The caller holds the same write lock as both launch entrypoints.
func (d *dispatcher) reclaimDispatchLaunch(ctx context.Context, id string, now time.Time) error {
	d.mu.Lock()
	route, ok := d.disk.OnDemand.Routes[id]
	if !ok || !reclaimableLaunch(route) || d.preparing[id] != nil {
		d.mu.Unlock()
		return nil
	}
	if route.Abandoned == nil {
		if route.RetryUntil.IsZero() {
			// Legacy reservations have no recorded age. Give them a full grace
			// period from the first sweep, including across daemon restarts.
			route.RetryUntil = now.Add(dispatchLaunchGrace)
			err := d.saveDemandRouteLocked(id, route)
			d.mu.Unlock()
			return err
		}
		if now.Before(route.RetryUntil) {
			d.mu.Unlock()
			return nil
		}
		// Keep the slot occupied and fence launch retries before cloud deletion.
		// A lost deletion response or a crash can then be reconciled on restart.
		route.Abandoned = &demandAbandonment{}
		if err := d.saveDemandRouteLocked(id, route); err != nil {
			d.mu.Unlock()
			return err
		}
	}
	d.mu.Unlock()

	cleanup, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	if err := d.onDemand.Reclaim(cleanup, route.Key); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	route.Abandoned = &demandAbandonment{Released: true}
	route.RetryUntil = time.Time{}
	return d.saveDemandRouteLocked(id, route)
}

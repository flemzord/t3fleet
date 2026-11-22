package proxy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"time"
)

// FixedPlacement manages stable, one-based VM slots. With Workers set to zero,
// existing fixed routes still resolve, but new threads use dedicated VMs.
// State is a local, non-waking lookup; only Prepare may provision a slot.
type FixedPlacement struct {
	Workers int
	State   func(slotKey string) string
	Lookup  func(slotKey string) (DispatchWorker, error)
	Prepare func(ctx context.Context, slotKey, checkoutKey, remote string) (DispatchWorker, string, error)
}

func (d *dispatcher) validateFixedPlacement() error {
	f := d.onDemand.Fixed
	if f == nil {
		return nil
	}
	if f.Workers < 0 || f.State == nil || f.Lookup == nil || f.Prepare == nil {
		return errors.New("invalid fixed fleet placement")
	}
	var retired map[string]demandRoute
	for id, r := range d.disk.OnDemand.Routes {
		if r.PoolSlot < 0 {
			return errors.New("invalid saved fixed slot")
		}
		if f.Workers == 0 || r.PoolSlot <= f.Workers || r.Abandoned != nil {
			continue
		}
		key := d.fixedSlotKey(r.PoolSlot)
		switch f.State(key) {
		case "deleted":
			continue
		case "absent":
			if !r.Ready {
				// No machine intent was persisted, so this launch cannot have
				// reached T3. Fence replay before dropping the unused slot.
				if retired == nil {
					retired = maps.Clone(d.disk.OnDemand.Routes)
				}
				r.Abandoned = &demandAbandonment{Released: true}
				retired[id] = r
				continue
			}
		}
		return fmt.Errorf("fixed slot %d (pool-%s) is still reserved; remove its VM explicitly before reducing dispatch.fixed.workers", r.PoolSlot, key[:12])
	}
	if retired != nil {
		next := d.disk
		demand := *next.OnDemand
		demand.Routes = retired
		next.OnDemand = &demand
		if err := d.saveDispatchState(next); err != nil {
			return errors.New("could not retire unused fixed launch reservations")
		}
		d.disk = next
	}
	return nil
}

func (d *dispatcher) fixedSlotKey(slot int) string {
	return demandKey(d.environmentID, "fixed-slot", strconv.Itoa(slot))
}

// Caller holds d.mu. Pending assignments count as load before a provider
// call, so simultaneous launches cannot all choose the same empty slot.
func (d *dispatcher) chooseFixedSlot() int {
	f := d.onDemand.Fixed
	type load struct{ active, assigned int }
	loads := make([]load, f.Workers+1)
	for _, route := range d.disk.OnDemand.Routes {
		if route.PoolSlot <= 0 || route.PoolSlot > f.Workers || route.Abandoned != nil || (route.Listing != nil && route.Listing.Archived) {
			continue
		}
		l := &loads[route.PoolSlot]
		l.assigned++
		if !route.Ready || route.Listing == nil || fixedThreadBusy(route.Listing.Thread) {
			l.active++
		}
	}
	best, bestCold := 0, 0
	for slot := 1; slot <= f.Workers; slot++ {
		cold := 1
		switch f.State(d.fixedSlotKey(slot)) {
		case "ready":
			cold = 0
		case "absent", "stopped":
		default:
			continue
		}
		a, b := loads[slot], loads[best]
		if best == 0 || cmp.Or(cmp.Compare(a.active, b.active), cmp.Compare(cold, bestCold), cmp.Compare(a.assigned, b.assigned)) < 0 {
			best, bestCold = slot, cold
		}
	}
	return best
}

func fixedThreadBusy(thread map[string]any) bool {
	if thread["activeRunId"] != nil || thread["activityRunStatus"] != nil || thread["pendingRuntimeRequest"] != nil {
		return true
	}
	switch thread["status"] {
	case "idle", "completed", "interrupted", "failed", "cancelled", "rolled_back":
		return false
	default:
		return true
	}
}

func (d *dispatcher) lookupRoute(r demandRoute) (DispatchWorker, error) {
	if r.PoolSlot != 0 {
		key := d.fixedSlotKey(r.PoolSlot)
		if d.onDemand.Fixed.State(key) == "deleted" {
			return DispatchWorker{}, errWorkerLost
		}
		return d.onDemand.Fixed.Lookup(key)
	}
	return d.onDemand.Lookup(r.Key)
}

func (d *dispatcher) prepareRoute(ctx context.Context, r demandRoute, p demandProject) (DispatchWorker, string, error) {
	if r.PoolSlot != 0 {
		projectKey := demandKey(d.environmentID, "fixed-project", p.ID)
		return d.onDemand.Fixed.Prepare(ctx, d.fixedSlotKey(r.PoolSlot), projectKey, p.Remote)
	}
	return d.onDemand.Provision(ctx, r.Key, p.Remote)
}

var errWorkerIdle = errors.New("fixed worker stopped for inactivity")

func fixedSubscription(tag any) bool {
	switch tag {
	case "orchestration.subscribeThread", "orchestration.subscribeCommandOutput", "orchestration.subscribePromptSuggestion":
		return true
	default:
		return false
	}
}

// Shared workers keep one project checkout and isolate every thread in its
// own worktree. A caller's root branch becomes the worktree's base.
func fixedWorkspaceStrategy(strategy map[string]any) map[string]any {
	if strategy["type"] == "worktree" {
		return strategy
	}
	branch, _ := strategy["branch"].(string)
	return map[string]any{"type": "worktree", "baseRef": cmp.Or(branch, "HEAD")}
}

// The HTTP shell omits archived rows. A confirmed native archive still has to
// move the cached row out of the active list, even when the worker stops next.
func (d *dispatcher) noteFixedArchive(id string, archived bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	route := d.disk.OnDemand.Routes[id]
	if route.PoolSlot == 0 || route.Listing == nil || route.Listing.Archived == archived {
		return nil
	}
	listing := *route.Listing
	listing.Archived = archived
	listing.Thread = maps.Clone(listing.Thread)
	if archived {
		listing.Thread["archivedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
	} else {
		listing.Thread["archivedAt"] = nil
	}
	route.Listing = &listing
	return d.saveDemandRouteLocked(id, route)
}

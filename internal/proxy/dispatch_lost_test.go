package proxy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/internal/state"
)

func TestLostReadyWorkerReleasesCapacity(t *testing.T) {
	t.Parallel()
	const lostID, liveID, archivedID, newID, projectID = "lost-thread", "live-thread", "archived-thread", "new-thread", "project"
	coldFailure := errors.New("worker never became ready")
	statusFailure := errors.New("provider status unknown")
	var checked, provisioned []string
	lost := map[string]bool{}
	var lostErr error
	var cancelSweep context.CancelFunc
	s, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p, err := NewDispatchOnDemand("http://127.0.0.1:38881", s, DispatchOnDemand{
		Controller:  DispatchWorker{Resolve: func(context.Context) (Upstream, error) { return Upstream{}, nil }},
		ProjectRoot: t.TempDir(), MaxWorkers: 2,
		Provision: func(_ context.Context, key, _ string) (DispatchWorker, string, error) {
			provisioned = append(provisioned, key)
			return DispatchWorker{}, "", coldFailure
		},
		Lookup: func(string) (DispatchWorker, error) { return DispatchWorker{}, nil },
		// Default configuration keeps workers when their threads are archived.
		Release: nil,
		Lost: func(_ context.Context, key string) (bool, error) {
			checked = append(checked, key)
			if lost[key] && lostErr == nil && cancelSweep != nil {
				cancelSweep()
			}
			return lost[key], lostErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := p.dispatch
	ctx := t.Context()
	d.disk.OnDemand.Projects[projectID] = demandProject{ID: projectID, Title: "Project", Remote: "https://github.com/octocat/Hello-World.git"}
	if err := d.store.Save(d.disk); err != nil {
		t.Fatal(err)
	}
	for id, route := range map[string]demandRoute{
		lostID:     {Key: "lost-key", ProjectID: projectID, Ready: true, Peer: &peerRoute{Grant: "grant", WorkerToken: "secret"}},
		liveID:     {Key: "live-key", ProjectID: projectID, Ready: true},
		archivedID: {Key: "archived-key", ProjectID: projectID, Ready: true, Archive: &demandArchive{Released: true}},
	} {
		if err := d.saveDemandRouteLocked(id, route); err != nil {
			t.Fatal(err)
		}
	}
	attempt := func(id string) error {
		unlock := d.lockDemandWrite(id)
		defer unlock()
		_, err := d.prepareDemandWorker(ctx, id, projectID)
		return err
	}
	requireFull := func() {
		t.Helper()
		if err := attempt(newID); err == nil || !strings.Contains(err.Error(), "dispatch worker limit") {
			t.Fatalf("expected a full pool, got %v", err)
		}
	}
	mustNotReport := func(err error) { t.Fatal(err) }

	// An uncertain provider answer keeps every reservation.
	lostErr = statusFailure
	var reported error
	d.releaseLostWorkers(ctx, func(err error) { reported = err })
	if !errors.Is(reported, statusFailure) || d.demandRoute(lostID).Lost != nil {
		t.Fatalf("uncertain status released a worker: %v %+v", reported, d.demandRoute(lostID))
	}
	requireFull()

	// A worker the provider still has keeps its slot. A busy thread is skipped.
	lostErr = nil
	unlock := d.lockDemandWrite(lostID)
	lost["lost-key"] = true
	d.releaseLostWorkers(ctx, mustNotReport)
	unlock()
	if d.demandRoute(lostID).Lost != nil || d.demandRoute(liveID).Lost != nil {
		t.Fatal("released a live or locked worker")
	}
	requireFull()

	// The real cleanup loop checks and releases missing workers even though
	// delete-on-archive is disabled. Cancel after the first confirmed loss.
	if d.handlesDemandArchive(liveID) {
		t.Fatal("archiving a live worker would delete it")
	}
	sweep, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cancelSweep = cancel
	p.RunDispatchCleanup(sweep, mustNotReport)
	route := d.demandRoute(lostID)
	if route.Lost == nil || !route.Lost.Released || route.Peer == nil || route.Peer.WorkerToken != "" || route.Peer.Grant != "grant" {
		t.Fatalf("lost route was not released cleanly: %+v", route)
	}
	if _, err := d.threadWorker(lostID); !errors.Is(err, errWorkerLost) {
		t.Fatalf("lost route still resolves a worker: %v", err)
	}
	if err := attempt(newID); !errors.Is(err, coldFailure) || len(provisioned) != 1 {
		t.Fatalf("freed capacity was not available: %v %v", err, provisioned)
	}
	if err := attempt(lostID); !errors.Is(err, errWorkerLost) {
		t.Fatalf("released lost route could be relaunched: %v", err)
	}
	for _, key := range checked {
		if key == "archived-key" {
			t.Fatal("checked an archived route")
		}
	}
}

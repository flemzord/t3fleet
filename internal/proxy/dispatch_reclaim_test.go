package proxy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/internal/state"
)

func TestAbandonedDispatchLaunchReleasesCapacityAfterRestart(t *testing.T) {
	t.Parallel()
	const oldID, otherID, projectID = "old-launch", "new-launch", "project"
	coldFailure := errors.New("worker never became ready")
	unconfirmed := errors.New("provider deletion not confirmed")
	var provisioned, reclaimed []string
	failedCleanup := true
	opts := DispatchOnDemand{
		Controller:  DispatchWorker{Resolve: func(context.Context) (Upstream, error) { return Upstream{}, nil }},
		ProjectRoot: t.TempDir(), MaxWorkers: 1,
		Provision: func(_ context.Context, key, _ string) (DispatchWorker, string, error) {
			provisioned = append(provisioned, key)
			return DispatchWorker{}, "", coldFailure
		},
		Lookup: func(string) (DispatchWorker, error) { t.Fatal("unexpected lookup"); return DispatchWorker{}, nil },
		Reclaim: func(_ context.Context, key string) error {
			reclaimed = append(reclaimed, key)
			if failedCleanup {
				return unconfirmed
			}
			return nil
		},
	}
	dir := t.TempDir()
	restart := func() (*dispatcher, func()) {
		s, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		close := func() { _ = s.Close() }
		t.Cleanup(close)
		p, err := NewDispatchOnDemand("http://127.0.0.1:38881", s, opts)
		if err != nil {
			t.Fatal(err)
		}
		return p.dispatch, close
	}
	d, close := restart()
	ctx := t.Context()
	d.disk.OnDemand.Projects[projectID] = demandProject{ID: projectID, Title: "Project", Remote: "https://github.com/octocat/Hello-World.git"}
	if err := d.store.Save(d.disk); err != nil {
		t.Fatal(err)
	}
	attempt := func(id string) error {
		unlock := d.lockDemandWrite(id)
		defer unlock()
		_, err := d.prepareDemandWorker(ctx, id, projectID)
		return err
	}
	requireLimit := func() {
		if err := attempt(otherID); err == nil || !strings.Contains(err.Error(), "dispatch worker limit") {
			t.Fatalf("expected a full pool, got %v", err)
		}
	}
	if err := attempt(oldID); !errors.Is(err, coldFailure) {
		t.Fatalf("expected a failed launch, got %v", err)
	}
	requireLimit()
	first := d.demandRoute(oldID)
	mustNotReport := func(err error) { t.Fatal(err) }
	d.reclaimDispatchLaunches(ctx, first.RetryUntil.Add(-time.Second), mustNotReport)
	unlock := d.lockDemandWrite(oldID)
	d.reclaimDispatchLaunches(ctx, first.RetryUntil, mustNotReport)
	unlock()
	if len(reclaimed) != 0 {
		t.Fatal("reclaimed a retryable or locked launch")
	}

	var reported error
	d.reclaimDispatchLaunches(ctx, first.RetryUntil, func(err error) { reported = err })
	if !errors.Is(reported, unconfirmed) {
		t.Fatalf("expected provider uncertainty to be reported, got %v", reported)
	}
	requireLimit()
	close()
	d, _ = restart()
	if err := attempt(oldID); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("launch replay was not fenced during cleanup, got %v", err)
	}

	failedCleanup = false
	d.reclaimDispatchLaunches(ctx, first.RetryUntil.Add(time.Minute), mustNotReport)
	if err := attempt(oldID); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("launch replay was not fenced after release, got %v", err)
	}
	if err := attempt(otherID); !errors.Is(err, coldFailure) {
		t.Fatalf("a new launch could not reach the provisioner after recovery: %v", err)
	}
	if !reflect.DeepEqual(reclaimed, []string{first.Key, first.Key}) ||
		!reflect.DeepEqual(provisioned, []string{first.Key, d.demandRoute(otherID).Key}) {
		t.Fatalf("unexpected backend calls: provisioned=%v reclaimed=%v", provisioned, reclaimed)
	}
}

func TestReclaimDispatchLaunchesStartsGraceForLegacyAndPreservesReadyRoutes(t *testing.T) {
	t.Parallel()
	s, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var reclaimed []string
	p, err := NewDispatchOnDemand("http://127.0.0.1:38881", s, DispatchOnDemand{
		Controller:  DispatchWorker{Resolve: func(context.Context) (Upstream, error) { return Upstream{}, nil }},
		ProjectRoot: t.TempDir(), MaxWorkers: 2,
		Provision: func(context.Context, string, string) (DispatchWorker, string, error) {
			return DispatchWorker{}, "", nil
		},
		Lookup:  func(string) (DispatchWorker, error) { return DispatchWorker{}, nil },
		Reclaim: func(_ context.Context, key string) error { reclaimed = append(reclaimed, key); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	d := p.dispatch
	now := time.Now().UTC()
	for id, route := range map[string]demandRoute{
		"legacy": {Key: "old-format"},
		"ready":  {Key: "may-have-native-work", Ready: true, RetryUntil: now.Add(-time.Hour)},
	} {
		if err := d.saveDemandRouteLocked(id, route); err != nil {
			t.Fatal(err)
		}
	}
	report := func(err error) { t.Fatal(err) }
	d.reclaimDispatchLaunches(t.Context(), now, report)
	if until := d.demandRoute("legacy").RetryUntil; len(reclaimed) != 0 || !until.Equal(now.Add(dispatchLaunchGrace)) {
		t.Fatalf("legacy launch was not given a full grace period: %v, %v", until, reclaimed)
	}
	d.reclaimDispatchLaunches(t.Context(), now.Add(dispatchLaunchGrace), report)
	if !reflect.DeepEqual(reclaimed, []string{"old-format"}) || !d.demandRoute("legacy").Abandoned.Released {
		t.Fatalf("unexpected reclaimed launches: %v", reclaimed)
	}
}

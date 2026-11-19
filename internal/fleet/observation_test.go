package fleet

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/pkg/provider"
)

// Embedding a nil Provider makes any accidental lifecycle call fail the test.
type observationProvider struct {
	provider.Provider
	read func(context.Context, []string) (map[string]string, error)
}

func (p observationProvider) ObserveStates(ctx context.Context, ids []string) (map[string]string, error) {
	return p.read(ctx, ids)
}

func TestObservedListKeepsLocalStateAndIsolatesAccounts(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	read := make(chan struct{})
	primary := observationProvider{read: func(_ context.Context, ids []string) (map[string]string, error) {
		if !slices.Equal(ids, []string{"shared", "missing"}) {
			t.Errorf("unexpected primary worker IDs: %v", ids)
		}
		close(read)
		return map[string]string{"shared": "stopped", "unmanaged": "idle"}, nil
	}}
	backup := observationProvider{read: func(ctx context.Context, ids []string) (map[string]string, error) {
		if !slices.Equal(ids, []string{"shared"}) {
			t.Errorf("unexpected backup worker IDs: %v", ids)
		}
		<-ctx.Done()
		return map[string]string{"shared": "running"}, ctx.Err()
	}}
	now := time.Date(2026, 11, 19, 17, 0, 0, 0, time.UTC)
	m := Manager{opts: Options{
		Providers: map[string]provider.Provider{"primary": primary, "backup": backup},
		Now:       func() time.Time { return now },
	}, machines: map[string]*machineHandle{}}
	for _, machine := range []Machine{
		{Name: "a", Provider: "primary", RemoteID: "shared", State: "ready"},
		{Name: "b", Provider: "primary", RemoteID: "missing", State: "ready"},
		{Name: "c", Provider: "backup", RemoteID: "shared", State: "ready"},
		{Name: "d", Provider: "primary", State: "creating"},
		{Name: "e", Provider: "other", RemoteID: "shared", State: "ready"},
	} {
		m.machines[machine.Name] = &machineHandle{record: record{Machine: machine}}
	}
	local := m.List()
	go func() {
		select {
		case <-read:
			cancel()
		case <-ctx.Done():
		}
	}()
	rows := m.ObservedList(ctx)
	if len(rows) != len(local) || !slices.Equal(local, m.List()) {
		t.Fatal("observation changed local inventory")
	}
	for i, want := range []string{"stopped", "unknown", "unknown", "unknown", ""} {
		if rows[i].Machine != local[i] || rows[i].ProviderState != want {
			t.Errorf("row %d: got %+v, want local machine plus %q", i, rows[i], want)
		}
		if i == 0 && !rows[i].ProviderObservedAt.Equal(now) || i != 0 && !rows[i].ProviderObservedAt.IsZero() {
			t.Errorf("row %d: unexpected observation time %v", i, rows[i].ProviderObservedAt)
		}
	}
}

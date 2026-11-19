package fleet_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/flemzord/t3fleet/internal/fleet"
	"github.com/flemzord/t3fleet/pkg/provider"
)

type legacyAccountProvider struct {
	*dispatchProvider
	lookups int
}

func (p *legacyAccountProvider) Get(ctx context.Context, id string) (provider.Machine, error) {
	p.lookups++
	return p.dispatchProvider.Get(ctx, id)
}

func TestLegacyWorkersRequireProofOfAccountBeforeMigration(t *testing.T) {
	for _, tc := range []struct {
		name, state, id, remoteName string
		getErr                      error
		accept                      bool
	}{
		{name: "verified allocation", state: "ready", id: "remote-1", remoteName: "t3fleet-worker", accept: true},
		{name: "same id other worker", state: "ready", id: "remote-1", remoteName: "t3fleet-unrelated"},
		{name: "not found in configured account", state: "stopped", id: "remote-1", getErr: provider.ErrNotFound},
		{name: "missing credentials", state: "ready", id: "remote-1", getErr: errors.New("credential unavailable")},
		{name: "unknown outcome", state: "creating"},
		{name: "never attempted", state: "planned", accept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFleet(t)
			if err := f.m.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			adapter := &legacyAccountProvider{dispatchProvider: &dispatchProvider{mockProvider: f.p, driver: "boat", scope: "newly-pinned-org", getErr: tc.getErr}}
			if tc.id != "" {
				f.p.machines[tc.id] = provider.Machine{ID: tc.id, Name: tc.remoteName, State: provider.StateStopped}
			}
			f.options.Providers = map[string]provider.Provider{"boat": adapter}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := ln.Addr().(*net.TCPAddr).Port
			if err := ln.Close(); err != nil {
				t.Fatal(err)
			}
			record := map[string]any{"id": strings.Repeat("a", 32), "name": "t3fleet-worker", "provider": "boat", "state": tc.state, "remoteId": tc.id, "port": port}
			if err := f.store.Save(map[string]any{"schema": 1, "machines": []any{record}}); err != nil {
				t.Fatal(err)
			}
			manager, err := fleet.New(f.options)
			if manager != nil {
				defer func() { _ = manager.Close(t.Context()) }()
			}
			if (err == nil) != tc.accept {
				t.Fatalf("accept=%t, got %v", tc.accept, err)
			}
			wantLookups := 0
			if tc.id != "" {
				wantLookups = 1
			}
			if adapter.lookups != wantLookups || adapter.attempts != 0 || adapter.deletes != 0 || adapter.starts != 0 {
				t.Fatalf("unexpected migration operations: reads=%d creates=%d deletes=%d starts=%d", adapter.lookups, adapter.attempts, adapter.deletes, adapter.starts)
			}
			var state struct {
				Schema   int
				Machines []struct{ Scope string }
			}
			if err := f.store.Load(&state); err != nil {
				t.Fatal(err)
			}
			if !tc.accept && (state.Schema != 1 || state.Machines[0].Scope != "") {
				t.Fatal("unverified account persisted")
			}
			if tc.accept && state.Machines[0].Scope != "newly-pinned-org" {
				t.Fatal("verified account not persisted")
			}
		})
	}
}

package boat

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"testing"
)

func TestObserveStatesReadsScopedPagesForManagedIDsOnly(t *testing.T) {
	requests := 0
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/sandboxes" || r.Header.Get("X-Boat-Org") != "test-org" ||
			r.Header.Get("Authorization") != "Bearer test-secret" || r.URL.Query().Has("state") {
			t.Errorf("unexpected inventory request: %s %s", r.Method, r.URL)
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = io.WriteString(w, `{"ok":true,"sandboxes":[{"id":"other","state":"running"},{"id":"no-state"},{"id":"sleep","state":"archived"},{"id":"awake","state":"idle"}],"pageInfo":{"hasMore":true,"nextCursor":"next+&="}}`)
		case "next+&=":
			_, _ = io.WriteString(w, `{"ok":true,"sandboxes":[{"id":"restoring","state":"cloning"},{"id":"boot","state":"provisioned"},{"id":"stop","state":"archiving"},{"id":"future","state":"private data"}],"pageInfo":{"hasMore":false,"nextCursor":null}}`)
		default:
			t.Error("unexpected cursor")
		}
	})
	got, err := c.ObserveStates(t.Context(), []string{"awake", "sleep", "restoring", "boot", "stop", "future", "missing", "no-state"})
	want := map[string]string{"awake": "idle", "sleep": "stopped", "restoring": "cloning", "boot": "starting", "stop": "stopping", "future": "unknown"}
	if err != nil || !maps.Equal(got, want) || requests != 2 {
		t.Fatalf("observations: %v, %v; requests: %d", got, err, requests)
	}
}

func TestObserveStatesDoesNotReadUnusedPages(t *testing.T) {
	requests := 0
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Has("cursor") {
			t.Error("unexpected additional page request")
		}
		_, _ = io.WriteString(w, `{"ok":true,"sandboxes":[{"id":"sleep","state":"archived"}],"pageInfo":{"hasMore":true,"nextCursor":"next"}}`)
	})
	if _, err := c.ObserveStates(t.Context(), nil); err != nil || requests != 0 {
		t.Fatalf("empty inventory: %v; requests: %d", err, requests)
	}
	got, err := c.ObserveStates(t.Context(), []string{"sleep", "sleep"})
	if err != nil || got["sleep"] != "stopped" || requests != 1 {
		t.Fatalf("observation: %v, %v; requests: %d", got, err, requests)
	}
}

func TestObserveStatesDiscardsPartialResultsOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unavailable", http.StatusTooManyRequests, `{"ok":false}`},
		{"missing list", http.StatusOK, `{"ok":true}`},
		{"cursor loop", http.StatusOK, `{"ok":true,"sandboxes":[],"pageInfo":{"hasMore":true,"nextCursor":"next"}}`},
		{"missing cursor", http.StatusOK, `{"ok":true,"sandboxes":[],"pageInfo":{"hasMore":true}}`},
		{"missing hasMore", http.StatusOK, `{"ok":true,"sandboxes":[],"pageInfo":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				if !r.URL.Query().Has("cursor") {
					_, _ = io.WriteString(w, `{"ok":true,"sandboxes":[{"id":"seen","state":"ready"}],"pageInfo":{"hasMore":true,"nextCursor":"next"}}`)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			if states, err := c.ObserveStates(t.Context(), []string{"seen", "missing"}); err == nil || states != nil {
				t.Fatalf("partial results: %v, %v", states, err)
			}
		})
	}
}

func TestObserveStatesHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := clientFor(t, func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	})
	if states, err := c.ObserveStates(ctx, []string{"worker"}); !errors.Is(err, context.Canceled) || states != nil {
		t.Fatalf("cancelled observations: %v, %v", states, err)
	}
}

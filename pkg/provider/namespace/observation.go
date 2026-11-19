package namespace

import (
	"context"
	"sync"

	"github.com/flemzord/t3fleet/pkg/provider"
)

var _ provider.StateObserver = (*Client)(nil)

// List omits the active instance ID. Fetch returns it for running Devboxes
// without waking them, but accepts only one ID per call.
func (c *Client) ObserveStates(ctx context.Context, ids []string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	jobs := make(chan string, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" && !seen[id] {
			jobs <- id
			seen[id] = true
		}
	}
	close(jobs)
	if len(seen) == 0 {
		return nil, nil
	}
	// Resolve and verify the tenant once for this inventory read, not once
	// per worker. No service or SSH credential is requested from a Devbox.
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	states := make(map[string]string, len(seen))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range min(4, len(seen)) {
		wg.Go(func() {
			for id := range jobs {
				if ctx.Err() != nil {
					return
				}
				var out response
				err := c.callWithToken(ctx, token, "Fetch", map[string]any{"id": id, "returnActivatedInstance": true}, &out)
				if err != nil {
					continue
				}
				machine, err := out.machine()
				if err == nil && machine.ID == id {
					mu.Lock()
					states[id] = string(machine.State)
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	// Each successful Fetch is an independent observation. A missing
	// worker or slow sibling must not invalidate completed reads.
	if len(states) == 0 && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return states, nil
}

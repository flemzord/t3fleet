package boat

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/flemzord/t3fleet/pkg/provider"
)

var _ provider.StateObserver = (*Client)(nil)

// ObserveStates lists the configured organization's sandboxes without a state
// filter. Only the requested IDs and their display-safe states leave the adapter.
func (c *Client) ObserveStates(ctx context.Context, ids []string) (map[string]string, error) {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			wanted[id] = true
		}
	}
	states := make(map[string]string, len(wanted))
	query := url.Values{"limit": {"200"}}
	cursors := map[string]bool{}
	for len(states) < len(wanted) {
		var page struct {
			Sandboxes *[]struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"sandboxes"`
			PageInfo *struct {
				HasMore    *bool  `json:"hasMore"`
				NextCursor string `json:"nextCursor"`
			} `json:"pageInfo"`
		}
		if err := c.request(ctx, http.MethodGet, "/sandboxes?"+query.Encode(), nil, nil, &page); err != nil {
			return nil, err
		}
		if page.Sandboxes == nil {
			return nil, errors.New("boat: incomplete inventory response")
		}
		for _, s := range *page.Sandboxes {
			if wanted[s.ID] && s.State != "" {
				states[s.ID] = observedState(s.State)
			}
		}
		info := page.PageInfo
		if info == nil { // Optional for an unpaginated response.
			break
		}
		if info.HasMore == nil || *info.HasMore != (info.NextCursor != "") || cursors[info.NextCursor] {
			return nil, errors.New("boat: invalid inventory pagination")
		}
		if !*info.HasMore {
			break
		}
		cursors[info.NextCursor] = true
		query.Set("cursor", info.NextCursor)
	}
	return states, nil
}

// Use the same lifecycle labels as the Boat CLI without passing arbitrary
// API text through to a terminal or using it as a JSON key.
func observedState(state string) string {
	switch state {
	case "provisioned":
		return "starting"
	case "archiving":
		return "stopping"
	case "archived":
		return "stopped"
	case "init", "provisioning", "cloning", "ready", "idle", "running",
		"error", "cancelled", "starting", "resuming", "stopping", "stopped", "deleted":
		return state
	default:
		return "unknown"
	}
}

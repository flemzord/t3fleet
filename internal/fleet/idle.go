package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/flemzord/t3fleet/pkg/provider"
)

var errUnknownActivity = errors.New("fleet: T3 activity is unknown; automatic stop refused")
var errAgentBusy = errors.New("fleet: T3 work is active; automatic stop refused")

func (m *Manager) idleLoop() {
	ticker := time.NewTicker(m.opts.IdleCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			_ = m.CheckIdle(m.ctx)
		}
	}
}

// CheckIdle makes one bounded sweep. It never creates, starts or deletes a VM.
// Busy lifecycle locks are skipped; failures hold the VM awake indefinitely.
func (m *Manager) CheckIdle(ctx context.Context) error {
	if m.opts.IdleTimeout == 0 {
		return nil
	}
	var result error
	for _, machine := range m.List() {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		h, err := m.handle(machine.ID)
		if err != nil || !h.operation.TryLock() {
			continue
		}
		func() {
			defer h.operation.Unlock()
			current, err := m.handle(machine.ID)
			if err != nil || current != h {
				return
			}
			r := m.copy(h)
			if h.activity == nil || r.State != "ready" || r.RemoteID == "" || r.Token == "" {
				return
			}
			checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			probe := func() (bool, error) {
				remote, err := m.opts.Providers[r.Provider].Get(checkCtx, r.RemoteID)
				if err != nil || remote.State != provider.StateRunning {
					return false, errUnknownActivity
				}
				return m.agentActivity(checkCtx, h)
			}
			err = h.activity.Check(m.opts.IdleTimeout, probe, func() error {
				// A second fresh read immediately precedes stop. Useful proxy work is
				// excluded by Activity.Check throughout both reads and the provider stop.
				busy, err := probe()
				if err != nil {
					return err
				}
				if busy {
					return errAgentBusy
				}
				if err := checkCtx.Err(); err != nil {
					return err
				}
				return m.stopLocked(checkCtx, h, "idle-timeout")
			})
			result = errors.Join(result, err)
		}()
	}
	return result
}

func (m *Manager) agentActivity(ctx context.Context, h *machineHandle) (bool, error) {
	r := m.copy(h)
	endpoint, err := m.opts.Providers[r.Provider].Connect(ctx, r.RemoteID, 3773)
	if err != nil {
		return false, errUnknownActivity
	}
	defer closeEndpoint(endpoint)
	client, base, err := endpointClient(endpoint)
	if err != nil {
		return false, errUnknownActivity
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return false, errUnknownActivity
	}
	applyEndpoint(req, endpoint, "/api/orchestration/shell")
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set("X-T3-Orchestration-Protocol", "2")
	response, err := client.Do(req)
	if err != nil {
		return false, errUnknownActivity
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return false, errUnknownActivity
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return false, errUnknownActivity
	}
	return shellBusy(data)
}

// The same busy markers used by t3boat's watchdog, with stricter shape/status
// validation: missing, malformed or future status data cannot authorize a stop.
func shellBusy(data []byte) (bool, error) {
	var shell struct {
		Threads  *[]map[string]json.RawMessage `json:"threads"`
		Archived *[]map[string]json.RawMessage `json:"archivedThreads"`
	}
	if json.Unmarshal(data, &shell) != nil || shell.Threads == nil || shell.Archived == nil {
		return false, errUnknownActivity
	}
	for _, rows := range [][]map[string]json.RawMessage{*shell.Threads, *shell.Archived} {
		for _, row := range rows {
			if row == nil {
				return false, errUnknownActivity
			}
			for _, key := range []string{"activeRunId", "activityRunStatus", "pendingRuntimeRequest"} {
				raw, exists := row[key]
				if key == "activeRunId" && !exists {
					return false, errUnknownActivity
				}
				if exists && string(raw) != "null" {
					return true, nil
				}
			}
			if raw, exists := row["pendingBackgroundTasks"]; exists {
				var pending []json.RawMessage
				if string(raw) == "null" || json.Unmarshal(raw, &pending) != nil {
					return false, errUnknownActivity
				}
				if len(pending) > 0 {
					return true, nil
				}
			}
			if raw, exists := row["blockedOnThreadWait"]; exists {
				var blocked bool
				if json.Unmarshal(raw, &blocked) != nil {
					return false, errUnknownActivity
				}
				if blocked {
					return true, nil
				}
			}
			var status string
			if json.Unmarshal(row["status"], &status) != nil {
				return false, errUnknownActivity
			}
			switch status {
			case "queued", "preparing", "starting", "running", "waiting":
				return true, nil
			case "idle", "completed", "interrupted", "failed", "cancelled", "rolled_back":
			default:
				return false, errUnknownActivity
			}
		}
	}
	return false, nil
}

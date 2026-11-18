package fleet

import (
	"context"
	"errors"
	"path"
	"strings"

	"github.com/flemzord/t3fleet/internal/proxy"
	"github.com/flemzord/t3fleet/pkg/provider"
)

// LookupDispatchBoat resolves a reservation without creating a VM. A deleted
// member remains a tombstone; retrying the thread must not fork its history.
func (m *Manager) LookupDispatchBoat(key string) (proxy.DispatchWorker, error) {
	batch, err := m.Batch(key)
	if err != nil || len(batch.Machines) != 1 || batch.Machines[0].Provider != "boat" || batch.Machines[0].State == "deleted" {
		return proxy.DispatchWorker{}, errors.New("the reserved Boat worker is unavailable")
	}
	return m.DispatchWorker(batch.Machines[0].ID)
}

// ProvisionDispatchBoat reserves one member using the same durable operation ID
// as manual batch creation, then initializes a private checkout on that VM.
func (m *Manager) ProvisionDispatchBoat(ctx context.Context, key, remote string) (proxy.DispatchWorker, string, error) {
	batch, err := m.Batch(key)
	if errors.Is(err, ErrNotFound) {
		if len(key) != 32 {
			return proxy.DispatchWorker{}, "", ErrInvalid
		}
		batch, err = m.PrepareBatch(ctx, BatchRequest{ID: key, Count: 1, CreateRequest: CreateRequest{Name: "thread-" + key[:12], Provider: "boat"}})
	}
	if err != nil || len(batch.Machines) != 1 || batch.Machines[0].Provider != "boat" || batch.Machines[0].State == "deleted" {
		return proxy.DispatchWorker{}, "", errors.New("could not reserve the Boat worker for this thread")
	}
	h, err := m.handle(batch.Machines[0].ID)
	if err != nil {
		return proxy.DispatchWorker{}, "", err
	}
	h.operation.Lock()
	defer h.operation.Unlock()
	if current, err := m.handle(batch.Machines[0].ID); err != nil || current != h {
		return proxy.DispatchWorker{}, "", errors.New("the reserved Boat worker was removed")
	}
	if err := m.ensure(ctx, h, false); err != nil {
		return proxy.DispatchWorker{}, "", errors.New("the reserved Boat worker is not ready; retry the same thread")
	}
	r := m.copy(h)
	result, err := m.opts.Providers["boat"].Exec(ctx, r.RemoteID, provider.ExecRequest{
		Args: []string{"/bin/bash", "-lc", dispatchCheckoutScript, "t3fleet-dispatch", key, remote},
	})
	if err != nil || result.ExitCode != 0 {
		return proxy.DispatchWorker{}, "", errors.New("could not prepare the GitHub checkout on Boat; check repository access in the Boat environment")
	}
	cwd := strings.TrimSpace(result.Stdout)
	if !path.IsAbs(cwd) || path.Clean(cwd) != cwd || !strings.HasSuffix(cwd, "/.t3fleet/dispatch-workspaces/"+key+"/repo") ||
		strings.ContainsAny(cwd, "\r\n\x00") || len(cwd) > 4096 {
		return proxy.DispatchWorker{}, "", errors.New("Boat did not return the reserved repository path")
	}
	w, err := m.DispatchWorker(r.ID)
	return w, cwd, err
}

// Use a private staging directory and publish only after clone returns. The
// command never imports client credentials, and suppresses Git diagnostics
// because an installed credential helper may annotate them with secrets.
const dispatchCheckoutScript = `set -euo pipefail
umask 077
key="$1"
remote="$2"
[[ "$key" =~ ^[0-9a-f]{32}$ ]]
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
export GIT_TERMINAL_PROMPT=0
base="$HOME/.t3fleet/dispatch-workspaces/$key"
repo="$base/repo"
mkdir -p -- "$base"
if [[ -e "$repo" || -L "$repo" ]]; then
  [[ ! -L "$repo" && "$(git -C "$repo" config --local --get remote.origin.url)" == "$remote" ]]
else
  stage=$(mktemp -d "$base/clone.XXXXXXXX")
  trap 'rmdir -- "$stage" 2>/dev/null || true' EXIT
  if ! git -c protocol.file.allow=never clone -- "$remote" "$stage/repo" >/dev/null 2>&1; then
    rm -rf -- "$stage/repo"
    exit 1
  fi
  mv -T -- "$stage/repo" "$repo"
fi
printf '%s\n' "$repo"
`

package fleet

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/flemzord/t3fleet/internal/proxy"
	"github.com/flemzord/t3fleet/pkg/provider"
)

var errDispatchSwitchUncertain = errors.New("could not confirm the dispatch fallback selection; restart the daemon and retry this thread")

// LookupDispatchWorker resolves a reservation without allocating a machine.
// Deleted members stay tombstones so retrying a thread cannot fork its history.
func (m *Manager) LookupDispatchWorker(key string) (proxy.DispatchWorker, error) {
	machine, err := m.dispatchReservation(key)
	if err != nil {
		return proxy.DispatchWorker{}, err
	}
	return m.DispatchWorker(machine.ID)
}

func (m *Manager) dispatchReservation(key string) (Machine, error) {
	batch, err := m.Batch(key)
	if err != nil {
		return Machine{}, err
	}
	if len(key) != 32 || len(batch.Machines) != 1 ||
		batch.Machines[0].Name != managedName("thread-"+key[:12])+"-01" ||
		m.opts.Providers[batch.Machines[0].Provider] == nil || batch.Machines[0].State == "deleted" {
		return Machine{}, errors.New("the reserved dispatch worker is unavailable")
	}
	return batch.Machines[0], nil
}

// ProvisionDispatchWorker reserves a single worker per thread before talking
// to either provider. Namespace workers install the daemon's pinned runtime
// only if the base image doesn't already have the hook.
func (m *Manager) ProvisionDispatchWorker(ctx context.Context, key, remote string, providerOrder []string) (proxy.DispatchWorker, string, error) {
	machine, err := m.dispatchReservation(key)
	if errors.Is(err, ErrNotFound) {
		if len(key) != 32 || len(providerOrder) == 0 {
			return proxy.DispatchWorker{}, "", ErrInvalid
		}
		// A concurrent launch may already have advanced this reservation to a
		// fallback, so consult the saved identity even on a plan conflict.
		_, err = m.prepareBatch(ctx, BatchRequest{ID: key, Count: 1, CreateRequest: CreateRequest{Name: "thread-" + key[:12], Provider: providerOrder[0]}}, providerOrder)
		if err == nil || errors.Is(err, ErrConflict) {
			machine, err = m.dispatchReservation(key)
		}
	}
	if err != nil {
		return proxy.DispatchWorker{}, "", errors.New("could not reserve the worker for this thread")
	}
	h, err := m.handle(machine.ID)
	if err != nil {
		return proxy.DispatchWorker{}, "", err
	}
	h.operation.Lock()
	defer h.operation.Unlock()
	if current, err := m.handle(machine.ID); err != nil || current != h {
		return proxy.DispatchWorker{}, "", errors.New("the reserved dispatch worker was removed")
	}
	// Reservations from before providerOrder have no saved policy. Recover
	// the original opt-in only if the first provider has not been called.
	if r := m.copy(h); r.DispatchFallbacks == nil && r.State == "planned" && r.RemoteID == "" &&
		len(providerOrder) > 0 && r.Provider == providerOrder[0] {
		if err := m.update(h, func(r *record) { r.DispatchFallbacks = slices.Clone(providerOrder[1:]) }); err != nil {
			return proxy.DispatchWorker{}, "", errors.New("could not save the dispatch provider order")
		}
	}
	for {
		r := m.copy(h)
		err = m.ensure(ctx, h, false)
		// The provider must explicitly refuse its first attempt. Never switch
		// after a lost response, an earlier attempt, or successful allocation.
		if len(r.DispatchFallbacks) == 0 || r.State != "planned" || r.RemoteID != "" ||
			!errors.Is(err, provider.ErrCreateRejected) || ctx.Err() != nil {
			break
		}
		if err := m.advanceDispatchProvider(key, h); err != nil {
			return proxy.DispatchWorker{}, "", err
		}
	}
	r := m.copy(h)
	if err != nil {
		if errors.Is(err, errDispatchSwitchUncertain) {
			return proxy.DispatchWorker{}, "", err
		}
		if r.Provider == "namespace" {
			return proxy.DispatchWorker{}, "", fmt.Errorf("the reserved Namespace worker is not ready; retry the same thread: %w", err)
		}
		return proxy.DispatchWorker{}, "", fmt.Errorf("the reserved %s worker is not ready; retry the same thread", r.Provider)
	}
	result, err := m.opts.Providers[r.Provider].Exec(ctx, r.RemoteID, provider.ExecRequest{
		Args: []string{"/bin/bash", "-lc", dispatchCheckoutScript, "t3fleet-dispatch", key, remote},
	})
	if err != nil || result.ExitCode != 0 {
		return proxy.DispatchWorker{}, "", fmt.Errorf("could not prepare the GitHub checkout on %s; check repository access on the worker", r.Provider)
	}
	cwd := strings.TrimSpace(result.Stdout)
	if !path.IsAbs(cwd) || path.Clean(cwd) != cwd || !strings.HasSuffix(cwd, "/.t3fleet/dispatch-workspaces/"+key+"/repo") ||
		strings.ContainsAny(cwd, "\r\n\x00") || len(cwd) > 4096 {
		return proxy.DispatchWorker{}, "", errors.New("worker did not return the reserved repository path")
	}
	w, err := m.DispatchWorker(r.ID)
	return w, cwd, err
}

// The caller holds the machine operation lock. Save the replacement in both
// indexes before calling the next provider so manual operations and restarts
// also see the chosen provider.
func (m *Manager) advanceDispatchProvider(key string, h *machineHandle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.batches[key]
	if m.closed || len(old.IDs) != 1 || old.IDs[0] != h.record.ID ||
		h.record.State != "creating" || h.record.RemoteID != "" || len(h.record.DispatchFallbacks) == 0 {
		return errors.New("could not select the next dispatch provider")
	}
	next := h.record.DispatchFallbacks[0]
	template := m.opts.Templates[next]
	if m.opts.Providers[next] == nil || template == (provider.Template{}) {
		return fmt.Errorf("the reserved dispatch fallback %q is missing its provider configuration or template", next)
	}
	batch := old
	batch.Request.Provider = next
	batch.Request.Template = template
	m.batches[key] = batch
	if err := m.updateLocked(h, func(r *record) {
		r.Provider, r.Template, r.InstallRuntime, r.State = next, template, next == "namespace", "planned"
		r.DispatchFallbacks = r.DispatchFallbacks[1:]
	}); err != nil {
		m.batches[key] = old
		h.dispatchSwitchUncertain = true
		return errDispatchSwitchUncertain
	}
	return nil
}

const dispatchRuntimeScript = `set -euo pipefail
umask 077
if command -v t3fleet-runtime-info >/dev/null; then
  exec t3fleet-runtime-info
fi
profile="$HOME/.local/state/nix/profiles/t3fleet"
mkdir -p -- "${profile%/*}"
exec 7>"$profile.install.lock"
flock -x 7
if [[ ! -x "$profile/bin/t3fleet-runtime-info" ]]; then
  src=$(mktemp -d)
  trap 'rm -rf -- "$src"' EXIT
  mkdir -- "$src/nix"
  printf '%s' "$1" >"$src/flake.nix"
  printf '%s' "$2" >"$src/flake.lock"
  printf '%s' "$3" >"$src/nix/runtime.nix"
  printf '%s' "$4" >"$src/nix/t3-server.nix"
  printf '%s' "$5" >"$src/nix/runtime-tests.nix"
  nix --extra-experimental-features 'nix-command flakes' profile add --profile "$profile" --no-update-lock-file "path:$src#vm-tools" >&2
  rm -rf -- "$src"
  trap - EXIT
fi
exec 7>&-
export PATH="$profile/bin:$PATH"
exec "$profile/bin/t3fleet-runtime-info"
`

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

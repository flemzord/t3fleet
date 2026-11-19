package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/flemzord/t3fleet/internal/proxy"
	"github.com/flemzord/t3fleet/pkg/provider"
)

var errDispatchSwitchUncertain = errors.New("could not confirm the dispatch fallback selection; restart the daemon and retry this thread")

// Capture the provisioning parameters when reserving the thread. Each
// fallback has its own size vocabulary, template, and driver.
type dispatchTarget struct {
	Instance  string              `json:"instance"`
	Driver    string              `json:"driver"`
	Scope     string              `json:"scope,omitempty"`
	Template  provider.Template   `json:"template"`
	Size      string              `json:"size,omitempty"`
	Placement *provider.Placement `json:"placement,omitempty"`
	Shape     provider.Shape      `json:"shape,omitzero"`
}

func (m *Manager) dispatchTargets(instances []string) ([]dispatchTarget, error) {
	targets := make([]dispatchTarget, 0, len(instances))
	for _, name := range instances {
		host := m.opts.Providers[name]
		req := CreateRequest{Provider: name}
		shape, err := m.applyProviderDefaults(&req)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid machine shape for fallback %q", ErrInvalid, name)
		}
		if host == nil || req.Template == (provider.Template{}) {
			return nil, fmt.Errorf("%w: dispatch fallback %q is missing its provider configuration or template", ErrInvalid, name)
		}
		targets = append(targets, dispatchTarget{Instance: name, Driver: host.Name(), Scope: providerScope(host), Template: req.Template, Size: req.Size, Shape: shape, Placement: providerPlacement(host)})
	}
	return targets, nil
}

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

// ReclaimDispatchWorker also accepts a missing batch: the dispatcher records
// its route before the fleet persists an intent, and the fleet never calls a
// provider before persisting that intent.
func (m *Manager) ReclaimDispatchWorker(ctx context.Context, key string) error {
	return m.releaseDispatchWorker(ctx, key, true)
}

// ReleaseDispatchWorker deletes exactly the machine reserved for a dispatch
// thread. The batch keeps its deleted member so cleanup can be retried after
// a lost response or a restart without creating another machine.
func (m *Manager) ReleaseDispatchWorker(ctx context.Context, key string) error {
	return m.releaseDispatchWorker(ctx, key, false)
}

func (m *Manager) releaseDispatchWorker(ctx context.Context, key string, allowMissingBatch bool) error {
	if len(key) != 32 {
		return ErrInvalid
	}
	batch, err := m.Batch(key)
	if allowMissingBatch && errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(batch.Machines) != 1 {
		return errors.New("the dispatch reservation must contain one worker")
	}
	worker := batch.Machines[0]
	if worker.Name != managedName("thread-"+key[:12])+"-01" {
		return errors.New("the reservation does not identify a dedicated dispatch worker")
	}
	if worker.State == "deleted" {
		return nil
	}
	if m.opts.Providers[worker.Provider] == nil {
		return errors.New("the reserved dispatch provider is unavailable")
	}
	err = m.deleteMachine(ctx, worker.ID, true)
	if errors.Is(err, ErrNotFound) {
		// A concurrent operator deletion may have completed first.
		batch, lookupErr := m.Batch(key)
		if lookupErr == nil && len(batch.Machines) == 1 && batch.Machines[0].State == "deleted" {
			return nil
		}
	}
	return err
}

// DispatchWorkerLost retires the local reservation when the provider confirms
// the worker is already gone. It never deletes a remote machine. A stopped,
// busy or uncertain worker keeps its slot.
func (m *Manager) DispatchWorkerLost(ctx context.Context, key string) (bool, error) {
	batch, err := m.Batch(key)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(key) != 32 || len(batch.Machines) != 1 || batch.Machines[0].Name != managedName("thread-"+key[:12])+"-01" {
		return false, errors.New("the reservation does not identify a dedicated dispatch worker")
	}
	worker := batch.Machines[0]
	if worker.State == "deleted" {
		return true, nil
	}
	h, err := m.handle(worker.ID)
	if err != nil {
		return false, nil
	}
	// Never wait behind a running operation: the next sweep checks again.
	if !h.operation.TryLock() {
		return false, nil
	}
	defer h.operation.Unlock()
	r := m.copy(h)
	host := m.opts.Providers[r.Provider]
	if r.RemoteID == "" || host == nil {
		return false, nil
	}
	remote, err := host.Get(ctx, r.RemoteID)
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		return false, errors.New("fleet: provider status check failed")
	}
	if err == nil && remote.State != provider.StateDeleted {
		return false, nil
	}
	if err := m.removeMachineRecord(h); err != nil {
		return false, err
	}
	return true, nil
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
	// Old state specified only fallback names, or no policy at all. Only
	// upgrade an unattempted intent; an uncertain allocation must not move.
	if r := m.copy(h); r.FallbackPlan == nil && r.State == "planned" && r.RemoteID == "" {
		names := r.DispatchFallbacks
		if names == nil && len(providerOrder) > 0 && r.Provider == providerOrder[0] {
			names = providerOrder[1:]
		}
		targets, targetErr := m.dispatchTargets(names)
		if targetErr != nil {
			return proxy.DispatchWorker{}, "", targetErr
		}
		if err := m.update(h, func(r *record) { r.FallbackPlan, r.DispatchFallbacks = targets, nil }); err != nil {
			return proxy.DispatchWorker{}, "", errors.New("could not save the dispatch provider order")
		}
	}
	for {
		r := m.copy(h)
		err = m.ensure(ctx, h, false)
		// The provider must explicitly refuse its first attempt. Never switch
		// after a lost response, an earlier attempt, or successful allocation.
		if r.State != "planned" || r.RemoteID != "" || !errors.Is(err, provider.ErrCreateRejected) {
			break
		}
		if len(r.FallbackPlan) == 0 || ctx.Err() != nil {
			// Only the first attempt can establish that there was no earlier
			// allocation with a lost response.
			if saveErr := m.update(h, func(current *record) { current.State = "planned" }); saveErr != nil {
				return proxy.DispatchWorker{}, "", errors.New("could not save the dispatch provider rejection")
			}
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
		return proxy.DispatchWorker{}, "", fmt.Errorf("the worker reserved on %s is not ready; retry the same thread: %w", r.Provider, err)
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
		h.record.State != "creating" || h.record.RemoteID != "" || len(h.record.FallbackPlan) == 0 {
		return errors.New("could not select the next dispatch provider")
	}
	next := h.record.FallbackPlan[0]
	host := m.opts.Providers[next.Instance]
	if host == nil || host.Name() != next.Driver || (next.Scope != "" && providerScope(host) != next.Scope) {
		// The primary was conclusively rejected. Keep the intent reusable
		// (and reclaimable) after the operator restores the fallback.
		if err := m.updateLocked(h, func(r *record) { r.State = "planned" }); err != nil {
			return errors.New("could not save the dispatch provider rejection")
		}
		return fmt.Errorf("the reserved dispatch fallback requires the original provider %q with driver %q and the same account and endpoint", next.Instance, next.Driver)
	}
	batch := old
	batch.Request.Provider, batch.Request.Template, batch.Request.Size = next.Instance, next.Template, next.Size
	// Two aliases may share a vendor account. A different template or size
	// must not reuse the rejected request's idempotency key in that account.
	var op [16]byte
	if _, err := rand.Read(op[:]); err != nil {
		return errors.New("could not generate the dispatch fallback operation")
	}
	m.batches[key] = batch
	if err := m.updateLocked(h, func(r *record) {
		r.CreateOperation = hex.EncodeToString(op[:])
		r.Scope = providerScope(host)
		r.Provider, r.Driver, r.Template, r.Size, r.Shape, r.Placement = next.Instance, next.Driver, next.Template, next.Size, next.Shape, next.Placement
		r.InstallRuntime, r.State = next.Driver == "namespace", "planned"
		r.FallbackPlan = r.FallbackPlan[1:]
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
  [[ ! -L "$repo" && "$(git -C "$repo" config --local --get remote.origin.url)" == "$remote" ]] || exit 1
else
  stage=$(mktemp -d "$base/clone.XXXXXXXX")
  trap 'rmdir -- "$stage" 2>/dev/null || true' EXIT
  clone_opts=(-c protocol.file.allow=never)
  if [[ "$remote" == https://github.com/* ]] && command -v gh >/dev/null &&
     [[ "$(git ls-remote --get-url -- "$remote" 2>/dev/null)" == "$remote" ]]; then
    # Leave configured helpers (even an explicit empty reset) and URL rewrites
    # to Git. --get-url only expands the URL; it makes no network request.
    status=0
    git config --get-urlmatch credential.helper "$remote" >/dev/null 2>&1 || status=$?
    if [[ "$status" == 1 ]]; then
      clone_opts+=(-c 'credential.https://github.com.helper=!GH_PROMPT_DISABLED=1 gh auth git-credential')
    fi
  fi
  if ! git "${clone_opts[@]}" clone -- "$remote" "$stage/repo" >/dev/null 2>&1; then
    rm -rf -- "$stage/repo"
    exit 1
  fi
  mv -T -- "$stage/repo" "$repo"
fi
printf '%s\n' "$repo"
`

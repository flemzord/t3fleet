# Experimental single-environment dispatch

`t3fleet serve` can expose one additional T3 environment that automatically
places each new thread on a distinct machine. Pair with that environment once.
The Desktop sends ordinary T3 protocol v2 requests and does not supply a
provider or machine ID. The proxy owns placement and identity; each worker
retains its own T3 database, checkout and model session.

## Workers on demand

This mode starts with **zero worker VMs**. It starts a dedicated local T3
controller, with a separate home and state directory, for pairing and project
metadata only. The controller does not clone the repository or run an agent.

Start from the complete [Boat-only](../examples/dispatch-boat.json) or
[Boat with Namespace fallback](../examples/dispatch-boat-namespace.json)
configuration. Use the same file for the daemon and CLI commands.

`dispatchPOC.onDemand.maxWorkers` limits how many worker reservations the
dispatcher accepts. Omit it or set it to `0` to use the default of 8, or set
any positive integer (for example, `100`). Provider quotas still apply.

1. Configure each provider in `dispatchPOC.onDemand.providerOrder`. For Boat,
   set `templates.boat.snapshot` to a prepared T3 snapshot. Set
   `providers.boat.environment` if the VM needs that environment's repository
   and model credentials. For Namespace, set `templates.namespace.image`.
   Set an optional [VM size](getting-started.md#vm-size) for each provider.
2. Set `dispatchPOC.onDemand.t3Binary` to an absolute executable path for T3
   matching `expectedVersion`. Leave `idleTimeout` unset or at `"0"` and omit
   `workers`. Start `t3fleet serve --config PATH`.
3. Run `t3fleet dispatch pair --config PATH` and confirm **t3fleet**
   in T3 Desktop. From a GitHub project, choose **Add to another machine** and
   select this environment. Clear any selected ignored files; copying them
   across machines is outside this proof.
4. Choose the default Git branch and send a text message. The proxy reserves a
   worker using the configured provider order, starts it, clones the GitHub
   repository using credentials available on the worker, and forwards the first
   message. The folder shown at
   project registration represents the virtual project; the checkout itself
   lives on the worker. A second thread gets a different VM.

This mode supports one GitHub repository, text messages, and a single Codex
model (set `dispatchPOC.onDemand.model`; the default is `gpt-5.4`). The worker
still needs model access; the local catalog cannot verify it before allocation.
A saved pairing and assignment survive daemon restarts at the same origin.
A failed or interrupted launch retains its reservation during the retry window
so the same request can reconcile it without allocating a second VM. It still
counts toward the limit until cleanup is confirmed. Use `t3fleet list` to inspect
the managed machines.

The cold path waits for cloud allocation and checkout. Large repositories can
exceed Boat's private command deadline. Paste only a credential-free GitHub
HTTPS or SSH URL; the dispatcher never forwards desktop credentials. Keep the
same private state directory and listen address on restart. T3 will reconnect
to the same named environment without another pairing. An older fixed-pool pairing
has a different environment identity, even if you use the same port.

The on-demand canary started with zero Boat VMs, then paired and registered a
public GitHub project with the fleet still empty. It sent two first messages
through the pinned T3 protocol, observed two Boat VMs and checked the cloned
repository on both. The original token recovered the histories and accepted a
follow-up on the same worker after restarting the daemon. Codex had no login on
either fresh VM, so the real model runs did not complete. The local genuine-T3
canary completes both first turns and the follow-up with a deterministic ACP
replay agent, and checks recovery of an interrupted reservation without a new
allocation.

### Fall back to Namespace

Keep the Boat provider and snapshot. Merge these fields into the existing
configuration, replacing `example-org/t3-worker` with the name of your
registered Namespace image:

```json
{
  "providers": { "namespace": { "size": "s" } },
  "templates": { "namespace": { "image": "registered:example-org/t3-worker" } },
  "dispatchPOC": { "onDemand": { "providerOrder": ["boat", "namespace"] } }
}
```

Use a Namespace identity that can create Devboxes and access this image. The
`registered:` prefix selects the name registered with Namespace, even when
the name contains a slash.

The first entry in `providerOrder` is the primary. The remaining entries are
fallbacks, tried in order. Use `["namespace"]` for Namespace only, or
`["namespace", "boat"]` to put Namespace first. Configure a provider and template
for every entry. The list must be nonempty and each provider can occur once.
If omitted, the order defaults to `["boat"]`. The legacy
`namespaceFallback: true` still means `["boat", "namespace"]`; remove it when
setting `providerOrder`. Every new reservation starts with the first provider.
This setting does not apply to manual creation or the fixed worker pool.

Each provider attempt uses that provider's configured size. For example, when
Boat falls back to Namespace, the worker uses `providers.namespace.size`. The
daemon saves the selected preset with the attempt so retries keep that preset.

t3fleet advances only when the current provider confirms that its first attempt
did not allocate a worker. Explicit Boat billing, limit and capacity refusals
qualify. Namespace can confirm a local token failure before sending the create
request. A Namespace create response can include activation failures after
allocation, so a failed response from that API does not trigger fallback.
A lost response, timeout, or a returned machine ID keeps the current reservation.
The saved order and worker survive restarts. Configuration changes affect new
reservations. Keep the provider settings, credentials and templates that saved
reservations can still need.

If the image has no t3fleet runtime, the daemon uses Nix in the Devbox to
install the pinned T3 and agent CLIs bundled with t3fleet. The first worker
start needs network access to the Nix inputs and upstream T3 release, and
can take longer with a cold cache. A failed bootstrap retains the same worker
for a retry. Each Devbox needs its own model access and, for private projects,
GitHub access. The daemon does not copy credentials from Boat or Desktop.

### Launch from another T3 environment

On-demand dispatch also accepts T3 **Linked Environments** on the pinned
nightly. Enable **Settings → Labs → Linked Environments** on the source T3
server. After pairing the Desktop with `t3fleet`, register the repository as
above. Run `t3fleet dispatch pair --peer-access approval-required --config PATH`
to print a one-use code. From the source environment, open **Linked
Environments → Link** and use that code to link to the dispatch listener.
When both servers run on the same host, enter the
dispatch loopback address manually (for example `http://127.0.0.1:47733`);
the machine picker omits loopback routes. An address on another machine must
be reachable from the source T3 server. t3fleet itself binds only to loopback.

From a source thread, call `t3_project_list` on the linked environment for the
project ID. Use `t3_thread_launch` with that project ID and a first text
message in a root checkout or a new worktree. Two launches with different
request IDs reserve two different workers. Retry a launch with the same
`clientRequestId` to recover its original worker and thread. The selected
Codex model and repository credentials must be available on each worker.

The source can read, wait for, message, interrupt or organize its linked
threads. The controller validates native T3 grants, including expiry and
revocation. Use **Sharing & access** on `t3fleet` to revoke a link. Each worker
applies the link and caller mode limits and keeps
the native thread fence. Other links and Desktop-created threads are outside
that scope. Scratch launches, attachments, fleet-wide operations, and links
to the fixed worker pool are unsupported. Re-linking creates a new grant;
existing threads stay in the Desktop history under their original grant.

### Recover abandoned launches

An incomplete launch reserves a slot while its worker is being prepared.
Retry the same launch within 30 minutes of the last attempt to reuse the
reservation. After that grace period, a sweep runs once a minute and reclaims
incomplete launches that are no longer being prepared. Existing incomplete
reservations with no timestamp get 30 minutes from their first sweep.

The dispatcher refuses further retries after it records a launch as abandoned.
It frees the slot only after the provider confirms deletion or absence, or
when the fleet never recorded an allocation intent. If a provider never
returned a machine ID and the allocation outcome is unknown, the slot remains
reserved. An operator can reconcile that managed machine with `t3fleet start ID`
and then delete it; the next sweep completes the release. The sweep never
deletes a worker whose dispatch route was already ready: a native T3 launch
may have succeeded even if the reply was lost.

### Delete dedicated workers on archive

Set `dispatchPOC.onDemand.deleteWorkerOnArchive` to `true` and restart the
daemon to opt in. The setting is off by default. It applies to dedicated
on-demand workers, whether they were provisioned on Boat or Namespace. It
does not change archiving on directly paired machines or in a fixed pool.

Archive from Desktop or from the linked environment that owns the thread.
The dispatcher first confirms the archive on the worker, persists the
pending cleanup, then deletes that exact provider machine. Deletion is
permanent: the thread history and worktree stored only on the worker are
lost. Push or export anything you need before archiving. The archived
thread is no longer available to read or unarchive through the dispatcher.

A worker counts toward `maxWorkers` until provider deletion is confirmed and
the dispatcher records the release. If provider cleanup fails, the daemon
keeps the reservation and retries the saved deletion at startup and on its
minute sweep. For Boat, an explicit not-found response from sandbox
inspection also confirms the machine is gone, even if the asynchronous
deletion operation has not completed. An authorized archive retry can also
resume cleanup. Repeating a completed archive returns success without another
provider request. Retrying
the original launch cannot recreate the worker. The sweep handles only
deletions already saved by an explicit archive under this setting; it does
not touch previously archived threads that have no saved deletion.

### Release workers removed outside t3fleet

An archive needs the worker to confirm it. If a ready worker was removed
outside t3fleet, for example from the provider console, its thread can no
longer be archived and its slot would stay reserved. With
`deleteWorkerOnArchive` enabled, the daemon checks every ready, unarchived
worker every 10 minutes. When the provider confirms that the machine no
longer exists, the dispatcher fences the thread, removes the machine record
and frees the slot. A stopped worker, a provider error or a machine with a
running operation keeps its reservation until a later sweep. The thread then
reports that its worker no longer exists; start a new thread to continue.

The linked dispatch canary runs a genuine T3 source, controller and two
workers locally using the ACP replay agent. It checks worker separation,
follow-up, retry after proxy restart, and access isolation without Boat costs:

```sh
T3FLEET_T3_BINARY=/absolute/path/to/t3 \
T3FLEET_T3_SOURCE=/absolute/path/to/pinned/t3code \
  nix develop --command go test -race -v -run '^TestLiveLinkedDispatch$' ./internal/proxy
```

```sh
T3FLEET_T3_BINARY=/absolute/path/to/t3 \
T3FLEET_T3_SOURCE=/absolute/path/to/pinned/t3code \
  nix develop --command go test -race -v -run TestLiveOnDemandDispatch ./internal/proxy
```

## Fixed worker pool (original proof)

Use [the provider config example](../examples/config.json) to configure Boat,
Namespace, or both. Namespace accepts either a prepared OCI `image`, or a
`source` Devbox with its `snapshot` ID. Each selected image or snapshot must
include the pinned T3 runtime and hook.

1. Provision 2–8 **dedicated** machines with the existing `create --no-register`
   command. Use an initialized Boat machine and an initialized Namespace
   machine to exercise both providers. Prepare the same repository at the same
   absolute path on both workers. Configure the same T3 provider instance IDs
   and model options on all workers.
2. Stop the local daemon. Add the following block to its config, substituting
   the managed IDs from `t3fleet list`. Leave `idleTimeout` unset or set it to
   `"0"`.
3. Start `t3fleet serve --config PATH`, then run
   `t3fleet dispatch pair --config PATH`. Open the resulting one-use link
   privately and confirm **t3fleet** in T3. Do not pair the
   individual machines in Desktop for this test.
4. In the new environment, add the prepared path as an existing project.
   Open two threads under the same project and send a first message in each.
   Restart the daemon at the same address and send a follow-up in the second
   thread. The environment and thread assignment should survive.

```json
{
  "dispatchPOC": {
    "listen": "127.0.0.1:47731",
    "workers": ["MANAGED_BOAT_ID", "MANAGED_NAMESPACE_ID"]
  }
}
```

The first new thread takes the first worker, the second takes the next worker,
and so on. A reservation is persisted before anything is forwarded, and a retry
with the same thread ID keeps its original worker. The POC never releases a
reservation, even after a rejected launch, because the worker may already have
accepted the request. It does not allocate more VMs, skip an unavailable worker,
migrate an existing thread or replay writes. Use a fresh dedicated state and
worker pool to start another experiment.

The proxy mirrors existing-directory project creation to the workers. It polls
their shell snapshots while subscribed and merges their assigned threads under
one environment ID. Thread detail HTTP requests and thread RPC streams go
directly to the assigned worker; the proxy uses a full shell snapshot on
reconnect. This makes each worker's history authoritative and avoids combining
independent application event cursors. Polling can wake stopped workers, so the
POC disallows automatic idle stop. The desktop's virtual provider catalog is
read from the first worker. Editing projects, device/preview/terminal,
attachments, uploads, cross-thread operations, jobs started outside the proxy,
and reconciliation of shell and worker cursors for advanced lifecycle commands
are outside this proof. Unsupported requests fail rather than silently using
another worker.

To repeat the protocol canary locally, use the pinned T3 binary and the
matching upstream source tree (see `nix/t3-server.nix`). Bun runs the upstream
deterministic ACP test agent; no Claude/Codex credentials are needed.

```sh
T3FLEET_T3_BINARY=/absolute/path/to/t3 \
T3FLEET_T3_SOURCE=/absolute/path/to/pinned/t3code \
  nix develop --command go test -race -v -run TestLiveDispatchPOC ./internal/proxy
```

The test opens one client WebSocket through the virtual environment, checks
that two first messages complete on different genuine servers, observes the
unified list, then reopens the saved pairing after a proxy restart and checks
the follow-up and history.

On the pinned nightly (`0.1.11-nightly.20261117.4838`), the same protocol
exercise also ran against one real Boat sandbox and one real Namespace
Devbox. Both ACP replay turns completed, the unified shell listed both
threads, and a separate read of each worker showed only its assigned thread.
After restarting the local daemon, the same client token could read the
Namespace history and continue its thread without changing the Boat thread.
The desktop click-through is still unverified: an isolated installation of the
same nightly did not get through startup and onboarding during this exercise.
The cloud test used the desktop's protocol over HTTP/WebSocket and a replay
fixture, with no commercial model calls.

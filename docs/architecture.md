# Architecture

`t3fleet` is a CLI and a local daemon. It separates provider lifecycle from T3 protocol handling so a new cloud backend does not require another gateway implementation.

## Modules

```text
cmd/t3fleet             CLI, authenticated control API, process lifecycle
internal/config         strict non-secret configuration and runtime credential loading
internal/fleet          durable machine lifecycle, initialization, stable listener ownership
internal/proxy          T3 pairing, authorization, HTTP and WebSocket gateway
internal/state          private atomic JSON persistence and exclusive ownership lock
pkg/provider            public provider contract and optional capabilities
pkg/provider/boat       Boat HTTP API adapter
pkg/provider/namespace  Namespace Connect API and private SSH adapter
```

The public extension surface is `pkg/provider`. Storage records, CLI wire types, and T3 protocol internals are intentionally private to the application.

## Create-to-ready lifecycle

1. The CLI sends an authenticated create request to the loopback control API.
2. The fleet manager validates the name and provider and selects a prepared template.
3. The manager allocates a durable operation ID and local port, then saves the creation intent before calling the provider.
4. The provider creates the VM from the image or snapshot. Retries reuse that operation ID; a lost response must not silently create a second billable VM.
5. The manager records the remote ID and calls the image's preinstalled `t3fleet-runtime-info` hook.
6. The hook initializes a unique T3 data directory and service credential on that machine, starts the installed server if needed, and returns a small private JSON result.
7. The manager checks the exact T3 version and probes the descriptor through the provider's private connection.
8. Only then does the managed record become `ready`.

A failed initialization does not erase the remote ID. The machine remains visible for diagnosis, restart, or explicit deletion. An incomplete creation retains its operation ID so `start` can reconcile it. Deletion does not discard local recovery state until provider deletion succeeds.

## Batch lifecycle

`PrepareBatch` reserves deterministic names, distinct machine IDs and stable
listeners in one durable state transaction before cloud allocation. Batch plans
persist the resolved template and size plus member IDs. CLI workers use the
existing authenticated `start` operation with bounded concurrency; provider
adapters do not know about batches. Replaying an identical batch ID returns its
original plan. Deleted members remain tombstones and cannot be recreated by
resume. Planned members can be deleted locally before any allocation attempt;
ambiguous allocations require provider reconciliation before cleanup.

The admin API exposes authenticated `POST /v1/batches` (prepare/replay) and
`GET /v1/batches/ID` (read current member states). Preparation does not allocate
cloud machines. The CLI owns individual progress, concurrency and desktop
handoffs. Multi-delete snapshots an explicit managed-ID selection, validates
all IDs before deletion and uses the existing per-machine DELETE route.
See [bulk lifecycle](bulk-lifecycle.md) for recovery and confirmation behavior.

## On-demand provider selection

For a new thread, the dispatch proxy asks the fleet manager to reserve one
worker. The fleet saves its first choice and remaining `providerOrder` before
calling any provider. When an adapter reports `provider.ErrCreateRejected` on
the first creation attempt, the fleet atomically saves the next choice before
contacting that provider. An uncertain outcome keeps the reservation on the
same provider so a retry can reconcile it. Once a worker exists, later
readiness, checkout, or model errors do not move the thread.

A Namespace dispatch worker whose image lacks the runtime hook can install the
pinned runtime from the daemon's bundled Nix files. Manual creation and the
fixed pool retain their prepared-runtime requirement. See the
[dispatch guide](dispatch-poc.md#fall-back-to-namespace) for configuration.

## Stable T3 origin

Each managed machine has a separate loopback listener. T3 Code associates with that origin, not with a provider address. A resumed Namespace instance can have a new ID or gateway route without changing T3's local URL.

Public discovery uses a persisted descriptor and never wakes the machine. Authenticated requests for live data and WebSocket connections resolve a private upstream. Resolution ensures the machine is running and ready. The request owns the connection and closes it when finished.

- Boat exposes a token-gated HTTPS port. Its query credential stays inside the daemon.
- Namespace uses verified TLS to its authenticated gateway, native Go SSH within that connection, and a tunnel to the T3 server's loopback port.

The complete exchange is documented in [protocol.md](protocol.md).

## Separate identities

There are three independent authorization layers:

| Layer | Credential | Stored where |
| --- | --- | --- |
| CLI → daemon control API | Local random admin token | Private daemon state directory |
| T3 desktop → machine proxy | Pairing-derived client bearer token | Private daemon state |
| Proxy → remote T3 server | Per-machine service bearer token | Remote runtime directory and private daemon state |

Cloud account credentials authorize provider operations and private transport. Claude/Codex credentials authorize model use on the VM. Neither is interchangeable with a T3 pairing token.

Golden templates contain software, not these identities. Starting two machines from the same template must produce two independent T3 environments.

## Persistence and concurrency

One daemon owns a state directory under an exclusive process lock. State writes use private files, atomic replacement, and `fsync`. The manager persists a complete versioned state document. Per-machine operation locks serialize lifecycle changes; unrelated machines can operate independently.

The daemon restores saved listeners at startup. A saved port conflict causes startup failure rather than silently changing an address already paired with T3. State contains secrets and is not a diagnostics artifact or a reusable image input.

## Idle lifecycle policy

When `idleTimeout` is positive, the manager runs a cancellable sweep. Per-machine
operation locks and a proxy activity gate serialize useful requests with the
final T3 workload check and provider stop. Agent checks connect privately to
already-running machines without using the wake resolver. Unknown status keeps
them awake indefinitely. Stop reason/time are persisted with the machine state.

The opt-in proxy path holds background RPC subscriptions locally while asleep,
then opens a fresh upstream on useful work. It bounds pending requests and tracks
finite useful RPCs through their terminal response. It never acknowledges an
agent dispatch before the upstream does. Session authorization metadata is
cached separately from the service credential so authenticated health checks do
not wake stopped machines. See [idle lifecycle](idle-lifecycle.md) for the
read-to-stop race with work started outside this proxy and the absence of offline
chat, scheduled wakes and a remote watchdog.

## First-release operating model

The control API and machine listeners bind loopback. Linux and macOS hosts are the initial daemon targets. A daemon can run on an always-on host with carefully configured SSH forwarding to the T3 desktop computer, but this release does not provide public hosting, TLS certificate management, a browser UI, or autonomous phone wake access.

Forward the control port and every paired machine port to the same local port if using SSH. Do not expose the admin listener publicly. A future hosted mode needs an explicit external origin and transport/authentication design; changing a bind address is not that design.

## Scope deliberately deferred

The proxy does not replay cached chats while asleep. The opt-in idle policy classifies explicit background RPC shapes and checks T3 workload snapshots; unknown work or activity errors prevent sleep. Provider auto-stop behavior remains independent.

Snapshot capture is optional. Boat has an explicit capture API; Namespace's inspected Devbox API supports restore and fork from existing snapshots. The common provider interface does not pretend these are identical capabilities.

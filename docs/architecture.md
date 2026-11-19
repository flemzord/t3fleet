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

## One environment, many projects

The daemon starts a local T3 controller in its own private home. Desktop and linked T3 servers pair with one stable dispatch origin. The controller owns project metadata. Repository identity comes from the canonical Git remote; the dispatch catalog maps that identity to a stable T3 project ID. It does not need preconfigured projects or cloud capacity to register a repository.

## From first message to worker

1. The dispatcher checks the project, the capacity limit and any existing thread assignment. It saves a route before asking for a worker.
2. The fleet manager saves a machine intent, an operation ID and the ordered provider choices before any cloud create call. The instance name identifies an account and its settings; the driver type selects the Boat or Namespace adapter.
3. The adapter reconciles the operation, then creates a worker using the selected template and size. Only a confirmed refusal before allocation permits moving to a fallback. A lost response stays with the same instance.
4. The manager initializes a private T3 identity on the worker and probes the configured T3 version through the provider’s private transport. A Namespace worker can install the bundled runtime if required.
5. The worker clones the project if absent. The dispatcher seeds that project on the worker’s T3 server, forwards the launch, and sends subsequent actions to the same server. The worker owns the checkout, history and model session.

The local control API exposes inspection, pairing and manual rescue operations. Operators do not need to pre-create a pool or pair individual workers. See [dispatch](dispatch.md) for restart, archive and cleanup semantics.

## Stable T3 origin

The fleet dispatch origin presented to T3 stays stable when placement changes. Each managed worker also has a separate private loopback listener for operator recovery. A resumed Namespace instance can change its gateway route without changing the paired address.

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

## Deployment boundary

The dispatch, administration and per-machine listeners bind loopback. Use private forwarding with a stable port to reach dispatch from another host. Public hosting would need TLS, an explicit external origin and an operator authorization model. Stopping the daemon closes local connections without stopping or deleting cloud resources.

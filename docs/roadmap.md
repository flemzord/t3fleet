# Scope and roadmap

This is the first implementation of a native Go fleet daemon. A checked box in a local test suite is different from a provider canary or a desktop compatibility test; release notes should state each separately.

## Implemented first-release scope

- Native Boat HTTP and Namespace Devbox API adapters.
- Creation from prepared images or snapshots, inspect, start, stop, and delete.
- Explicit snapshot capture for Boat; snapshot restore/fork for Namespace.
- A prepared-image runtime contract with per-machine T3 identity and credential initialization.
- Stable per-machine loopback ports and durable client authorization.
- T3 pairing, authenticated HTTP/WebSocket relay, and scoped socket tickets.
- Automatic readiness/start resolution when live proxy traffic needs a stopped machine.
- Private atomic state, an exclusive ownership lock, strict configuration, and a local authenticated control API.
- Offline tests with the race detector, formatting checks, and `go vet`.

The [provider guides](providers-boat.md) and [protocol guide](protocol.md) define the supported contracts. A prepared template remains a prerequisite; the daemon does not install T3 from the network on each boot.

## Current limits

- **Local operating model:** loopback-only listeners; no supported Internet-facing service or browser UI.
- **Live proxy:** authenticated chat requests may wake a machine. Public discovery uses a persisted descriptor without waking it. There is no offline chat replay.
- **Explicit power management:** no agent-aware automatic sleep or schedule-based wake policy. Provider auto-stop rules can still apply.
- **Model authentication:** sign in remotely using the model vendor's flow; no login callback forwarding yet.
- **Protocol coverage:** no OAuth MCP linked-environment sessions or URL-only attachment credential rewriting/caching.
- **Snapshots differ:** Namespace does not expose the same explicit capture method used by Boat.
- **Compatibility is versioned:** a newer T3 desktop/server combination requires a new compatibility test and image pin.

## Next milestones

1. Establish repeatable, opt-in cloud canaries for both providers, including stop/resume and resource cleanup.
2. Automate clean golden-template publication with version manifests and compatibility checks.
3. Improve safe recovery and provider reconciliation after process/network failures.
4. Add remote model-login forwarding using the vendor's own login, without copying local credentials.
5. Add agent-aware idle detection, snapshot coordination, and wake scheduling as a separate policy module.
6. Design a hosted mode with stable external origins, TLS, explicit operator authentication, and phone access independent of a laptop.
7. Add an optional desktop UI after the headless lifecycle and protocol contract are stable.

More providers can be added through [the provider contract](adding-a-provider.md). A new backend should bring native lifecycle and private-transport evidence, rather than just a provider name in a configuration enum.

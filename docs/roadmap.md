# Scope and roadmap

This is the first implementation of a native Go fleet daemon. A checked box in a local test suite is different from a provider canary or a desktop compatibility test; release notes should state each separately.

## Implemented first-release scope

- Native Boat HTTP and Namespace Devbox API adapters.
- Creation from prepared images or snapshots, inspect, start, stop, and delete.
- Durable identical batches with bounded CLI concurrency, replay/resume, and explicit multi-delete selection.
- Explicit snapshot capture for Boat; snapshot restore/fork for Namespace.
- A runtime contract with per-machine T3 identity and credential initialization.
- Experimental [on-demand dispatch with ordered provider fallback](dispatch-poc.md#fall-back-to-namespace).
- Stable per-machine loopback ports and durable client authorization.
- Automatic desktop association handoff after `create`, with `--no-register` for headless use.
- T3 pairing, authenticated HTTP/WebSocket relay, and scoped socket tickets.
- Automatic readiness/start resolution when live proxy traffic needs a stopped machine.
- Optional configurable idle stop with authenticated agent checks and background RPC wake suppression.
- Private atomic state, an exclusive ownership lock, strict configuration, and a local authenticated control API.
- Offline tests with the race detector, formatting checks, and `go vet`.

The [provider guides](providers-boat.md) and [protocol guide](protocol.md) define the supported contracts. Manual creation requires a template with the runtime hook installed. On-demand Namespace workers can install the pinned runtime on first use if the image includes Nix.

## Current limits

- **Local operating model:** loopback-only listeners; no supported Internet-facing service or browser UI.
- **Live proxy:** authenticated chat requests may wake a machine. Public discovery uses a persisted descriptor without waking it. There is no offline chat replay.
- **Power management:** opt-in daemon-based idle stop; no remote watchdog, CPU-job protection or schedule-based wake policy. Provider auto-stop rules can still apply. See [idle lifecycle](idle-lifecycle.md).
- **Desktop association:** T3 still requires one confirmation per backend after the automatic handoff. Zero-click registration needs a supported T3 desktop integration; t3fleet does not edit its encrypted connection catalog.
- **Model authentication:** sign in remotely using the model vendor's flow; no login callback forwarding yet.
- **Protocol coverage:** no OAuth MCP linked-environment sessions or URL-only attachment credential rewriting/caching.
- **Snapshots differ:** Namespace does not expose the same explicit capture method used by Boat.
- **Compatibility is versioned:** a newer T3 desktop/server combination requires a new compatibility test and image pin.

## Next milestones

1. Maintain the [validated cloud canaries](validation.md) for both providers and automate scheduled runs with explicit billing controls.
2. Automate clean golden-template publication with version manifests and compatibility checks.
3. Improve safe recovery and provider reconciliation after process/network failures.
4. Add remote model-login forwarding using the vendor's own login, without copying local credentials.
5. Extend the conservative idle policy with remote watchdogs, non-agent workload protection, snapshot coordination and scheduled wakes.
6. Design a hosted mode with stable external origins, TLS, explicit operator authentication, and phone access independent of a laptop.
7. Add an optional desktop UI after the headless lifecycle and protocol contract are stable.

More providers can be added through [the provider contract](adding-a-provider.md). A new backend should bring native lifecycle and private-transport evidence, rather than just a provider name in a configuration enum.

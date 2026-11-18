# Validation record

Validated on 2026-11-18 with Go 1.27.3 and T3 server
`0.1.11-nightly.20261117.4838`. This records observed behavior, not a guarantee
for another desktop version, image, account, or region.

## Local checks

- `make lint`: formatted Go source and `go vet`.
- `make test`: all packages with the race detector; no cloud credentials.
- `make build`: CLI compilation.
- `nix build .#checks.aarch64-darwin.runtime`: the real generated runtime wrappers
  against a long-lived mock server, including start-lock inheritance, token
  isolation, and credential-free image checks.
- `TestLiveT3Compatibility`: an actual pinned T3 process in a disposable home;
  descriptor, OAuth association, service session, WebSocket RPC, and orchestration
  shell. This is protocol evidence, not a desktop UI test.

CLI tests verify that `create` requests pairing and opens T3 in order, while
`--no-register` skips both. Failed pairing or app launch preserves the allocated
machine identity and never logs its one-use credential. OS launch arguments are
checked for macOS, Linux, and Windows. These tests use an injected launcher; they
do not establish that a desktop user accepted the association.

Regression tests also cover HTTP/2 TLS negotiation with private endpoint cookies,
WebSocket redirect rejection, interrupted creation, partial allocation, durable
recovery, cancellation, and asynchronous deletion.

## Real cloud lifecycle

Both opt-in `TestCloudLifecycle` runs passed using current authenticated accounts
and dedicated test machines. Boat used a prepared named snapshot. Namespace used
an explicit completed snapshot of a stopped golden source Devbox. Both templates
contained the public Nix `vm-tools` profile on Linux amd64, including T3, Codex,
Claude Code, and `t3fleet-runtime-info`.

| Observed step | Boat | Namespace |
| --- | --- | --- |
| Create from prepared snapshot | Passed | Passed |
| Initialize a unique T3 identity without downloading packages | Passed | Passed |
| Verify descriptor, exact version, and service authorization | Passed | Passed |
| Exchange local pairing credential | Passed | Passed |
| Authenticated orchestration HTTP | Passed | Passed |
| Real T3 `server.getConfig` over relayed WebSocket | Passed | Passed |
| Stop and confirm provider state | Passed | Passed |
| Fetch public descriptor without waking the stopped machine | Passed | Passed |
| Resume and reuse the existing client authorization | Passed | Passed |
| Delete and confirm absence/terminal remote state | Passed | Passed |

The latest full runs completed in approximately 109 seconds for Boat and 55
seconds for Namespace, excluding Go startup. These are individual observations,
not performance targets. Runtime initialization is still required after boot;
preparation removes installation and downloads from that path.

Test VMs and failed temporary build resources were removed. Successful golden
sources remain stopped, with their snapshots retained for future machines.
Stopped machines and saved snapshots may still incur storage charges. No account
identifiers, credentials, private endpoints, or runtime output are stored here.

## Limits of this proof

- The reference Dockerfile is included, but a complete build and provider image
  optimization of that exact recipe was not completed. The public Nix tools and
  runtime were built and exercised on real Linux machines, then captured as clean
  provider snapshots. Full personal developer-image builds are separate.
- Linux arm64 is packaged but was not exercised by these cloud trials.
- No real Claude/Codex model login or inference was performed. Sign in on each
  resulting machine; golden templates contain no user model sessions.
- No T3 desktop click-through or phone access was tested. This release uses a
  local daemon and loopback endpoints; the computer running it must stay available.
- One earlier Boat private command failed while an immediate diagnostic succeeded.
  Its exact cause was not established. Later full runs passed. The adapter now
  exposes sanitized failure phases for future diagnosis; arbitrary commands are
  not automatically replayed.
- CI configuration is committed. No GitHub push or remote CI run is claimed by
  these local results.

For repeatable commands and cleanup rules, see
[Contributing](../CONTRIBUTING.md#optional-live-validation).

## Bulk lifecycle offline validation (2026-11-18)

The prepared bulk-lifecycle checkout was verified to start at local commit
`fa200e3e87c955a1cbaae62f81dd7b6212e813f4` before edits. Its Nix shell now
explicitly selects Go 1.27.3 through `GOTOOLCHAIN`; the locked nixpkgs Go is the
bootstrap compiler, and the first use may download the verified toolchain.
Previously the shell's automatic toolchain selection ran Go 1.27.0 from `go.mod`.

The following command passes on macOS arm64 with Go 1.27.3:

```sh
nix develop --command make lint test build
```

Live-test opt-in variables were explicitly unset for validation. `lint` checks
gofmt and runs `go vet`; `test` runs all packages with `-race -count=1`; `build`
writes `bin/t3fleet` inside this checkout only. The CLI help was also checked.
No cloud machine was allocated or deleted, and no real T3 desktop was opened.
The existing `mon-test*` resources, private configurations and other checkouts
were not used. No push, PR, merge or remote CI run is part of this evidence.

New fixtures verify full local name preflight, persistence failure without
allocation, stable IDs after restart, replay conflicts, identical template/size,
reconciliation after a lost create response, deleted-member tombstones,
cancellation, bounded concurrent starts and individual partial outcomes. They
also verify runtime/service/client credential isolation, rejection of a cloned
runtime identity, and a start/delete race on planned intents without orphaning
an allocation. CLI tests cover admin authentication, managed-ID selection,
deduplication, declined/empty/invalid/EOF selection, headless deletion, partial
delete failure, and pacing/cancellation of desktop handoffs with an injected
launcher. T3 confirmations and a real ten-VM batch remain unverified live.

## Idle lifecycle offline validation (2026-11-18)

Go 1.27.3 on macOS arm64 passes `nix develop --command make lint test build`
for the optional `idleTimeout` policy. Live cloud/T3 opt-in variables were unset.
The binary was built only in the prepared worktree. No real VM was stopped,
started, created or deleted; no private configuration or T3 catalog was changed.

Regression fixtures cover:

- Disabled-by-default configuration and strict duration validation.
- Agent runs, queued work, approvals, subagents, thread waits and background
  tasks blocking sleep, including archived threads.
- Unknown/malformed/oversized snapshots and failed authorization blocking sleep
  indefinitely, with a full grace period after checks recover.
- A fresh final workload recheck, in-flight proxy work, and the activity gate
  serializing new useful requests with stop.
- Failed stop retaining recoverable state, cancellation, daemon-loop shutdown,
  and probes not waking an externally stopped VM.
- Persisted stop reason/time, identical machine IDs/ports and client credentials
  after restart and wake; background auth/shell/media traffic not waking.
- Periodic service-auth refresh not extending the idle clock.
- Idle live subscriptions, local sleeping pings, transparent stream acknowledgements,
  same-socket wake, ordered requests and no duplicate message dispatch.
- Authentication and bounded buffers on the sleeping WebSocket path, conservative
  RPC/payload classification and exact JSON numeric precision.

The reference was t3boat commit `bd488c87c88b1b467709afaa3080d837483aafbb`.
Its remote watchdog, CPU checks, snapshot capture, database reads, scheduled
wake behavior and offline chat replay were not ported. The local policy's live
desktop behavior and the external-client read-to-stop race remain explicit
limits; see [idle lifecycle](idle-lifecycle.md).

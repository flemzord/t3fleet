# Automatic idle stop and wake

`serve` can stop idle managed machines without deleting or replacing them.
It keeps the same machine ID, provider ID, T3 identity, local port and client
pairings. This policy is disabled by default.

## Enable the policy

Add this top-level property to your existing configuration:

```json
{
  "idleTimeout": "30m"
}
```

This fragment is not a replacement for your provider/state configuration.
Restart `serve` with the new binary and the same configuration. The timeout
applies to all ready machines managed by this daemon, for both Boat and
Namespace. Durations use Go syntax, for example `5m`, `30m` or `1h`.
The minimum positive timeout is `1m`. Omit the field or use `"0"` to disable it.
Negative, malformed, numeric JSON and sub-minute values are rejected. Disabling
the policy restores the original live-proxy behavior, including background
reconnections that can wake stopped VMs.

The daemon checks machines roughly every 15 seconds, sequentially, with a
15-second per-machine deadline. Busy lifecycle locks are skipped. A slow machine
can delay checks of later machines. The timeout is a minimum idle window, not an
exact shutdown deadline. Daemon restart starts a full new idle window for
running records; it does not immediately stop a machine based on an old clock.

Only fully initialized `ready` records are eligible. Planned, failed, unresolved
and already-stopped machines are not allocated, reconciled or woken by the idle
sweep. Stopping the daemon cancels its sweep and does not stop cloud machines.

## What prevents automatic stop

The daemon reads the complete `/api/orchestration/shell` snapshot through the
provider's private transport, with the per-machine T3 service credential and
orchestration protocol 2. The probe does not use the wake resolver. It checks
both `threads` and `archivedThreads`, including subagents.

Any of these markers prevents stop and restarts the idle window:

- A non-null `activeRunId` or `activityRunStatus`.
- A `queued`, `preparing`, `starting`, `running` or `waiting` thread.
- A pending runtime request, including a user approval or question.
- Pending background tasks or a thread blocked waiting for another thread.

Missing or malformed arrays, missing required run/status data, unknown statuses,
an unavailable private endpoint, non-success HTTP, an oversized response or a
failed provider read all mean **unknown**. Unknown prevents automatic stop
indefinitely. It is never interpreted as an empty workload. After recovery, a
full new idle window must elapse.

An authenticated useful HTTP operation or RPC also holds the VM awake until its
response completes. An unrecognized RPC is treated as useful work. Long-lived
unknown RPCs, unknown WebSocket types, and live device/preview streams may keep
it awake conservatively. A useful request ending resets the idle window.
Periodic auth revalidation does not reset it. Invalid credentials cannot wake a
machine or reset its timer.

Immediately before stop, the daemon reads the workload again. Lifecycle locks
and the activity gate exclude new useful requests through this proxy until the
check and provider stop finish. Such a request waits, then wakes the same VM.
A failed stop is not claimed as completed and restarts the idle window.
`inspect ID` exposes `lastStopReason` (`idle-timeout` or `manual`) and
`lastStoppedAt` after a confirmed, durably saved stop. These describe the last
stop, even after a subsequent wake. Explicit `stop ID` retains its operator
controlled behavior; the idle guard does not turn it into a conditional stop.

## Background traffic and wake

An open RPC socket, transport pings, known status subscriptions and selected
background reads are not user activity. The proxy classifies explicit protocol-2
methods and relevant payload shapes; an unknown method remains useful work.

When asleep:

- Public discovery uses the saved descriptor.
- Client session checks use saved authorization metadata, after checking the
  client's own token. Service credentials are never returned to the client.
- RPC sockets remain local. Pings are answered by the proxy; subscriptions are
  held in a bounded queue without opening an upstream connection.
- Shell polling and device/preview reconnects return 503 instead of waking.
- The first useful RPC, such as `orchestration.dispatchCommand` with
  `message.dispatch`, wakes the VM. Pending requests then go upstream in their
  original order. A mutation is not acknowledged before the server answers it.

Buffers are limited to 256 outstanding requests and 1 MiB of retained request
bytes per socket. Overflow closes the socket. Disconnect or failed wake does
not cause automatic replay of a possibly executed mutation. The implementation
relays stream acknowledgements and continues sanitizing `directEndpoints`.

Use `start ID` to wake explicitly. Existing T3 pairings remain valid through an
idle stop and daemon restart, subject to their normal expiration. A record from
an older binary may lack saved session metadata: `start ID` refreshes it.

## Scope and safety limits

This is a daemon policy, not a VM watchdog. It only operates while `serve` runs.
Provider-native auto-stop rules remain independent and can act outside this
policy. No automatic snapshot, machine replacement or cloud deletion is added.

T3 has no atomic "freeze new agents and stop" contract used here. Two fresh
snapshots plus the proxy gate protect observed work and requests through this
proxy. They cannot lock out a scheduler, another client or a process that starts
work directly on the VM in the final read-to-stop gap. Avoid bypassing this
proxy when relying on its idle policy. Future scheduled runs do not wake a
stopped VM automatically. Work outside T3, such as a detached shell build, is
not detected by these agent markers. Keep the policy disabled for machines
that depend on those workloads.

There is no offline chat replay. While asleep, unserved background subscriptions
can remain loading until useful work or `start ID` wakes the VM. The proxy does
not fabricate server configuration, thread snapshots or successful commands.
Desktop behavior on a real idle-stop/wake cycle still requires a separate live
canary. Automated validation uses local fixtures only.

## t3boat reference

The implementation was studied from
[t3boat at bd488c87](https://github.com/paukraft/t3boat/tree/bd488c87c88b1b467709afaa3080d837483aafbb),
particularly its
[watchdog busy markers](https://github.com/paukraft/t3boat/blob/bd488c87c88b1b467709afaa3080d837483aafbb/apps/desktop/src-tauri/src/machine/watchdog.mjs#L208)
and [RPC classification](https://github.com/paukraft/t3boat/blob/bd488c87c88b1b467709afaa3080d837483aafbb/apps/desktop/src-tauri/src/proxy/classify.rs).

Its watchdog runs on each Boat VM, also checks CPU activity and future scheduled
work, saves snapshots, and coordinates offline chat capture with its proxy.
It eventually stops treating an unavailable agent check as busy after a grace
period. t3fleet instead keeps unknown activity awake indefinitely, runs the
policy in the local daemon, uses native provider APIs, and leaves CPU monitoring,
remote watchdogs, snapshot coordination, scheduled wakes and offline replay for
separate work. The MIT notice is preserved in
[third-party notices](../THIRD_PARTY_NOTICES.md).

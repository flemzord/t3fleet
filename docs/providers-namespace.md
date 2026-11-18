# Namespace provider

The Namespace adapter uses the native Devbox Connect API and a private SSH
connection through Namespace's authenticated TLS gateway. It does not spawn
`devbox`, `nsc`, `ssh`, or another CLI.

## Configuration

`namespace.New(namespace.Config{...})` accepts:

| Field | Default | Purpose |
| --- | --- | --- |
| `Endpoint` | `https://private-api.iad.namespaceapis.com` | Devbox control plane; independent of machine placement. |
| `Site` | `iad` | Site used when creating a Devbox from an image. |
| `VirtualCPU` | `4` | CPU count when a request has no named size. |
| `MemoryMB` | `8192` | Memory when a request has no named size. |
| `Architecture` | `amd64` | `amd64` or `arm64`; Linux is the initial supported guest OS. |
| `TokenSource` | `auth.LoadDefaults()` | Official Go SDK token source. |
| `HTTPClient` | Two-minute request timeout | Optional HTTP transport for Connect calls. |
| `DialSSH` | Native gateway connector | Injectable connection seam, primarily for offline tests. |

Named sizes `s`, `m`, `l`, and `xl` resolve to 4/8/16/32 CPUs and
8/16/32/64 GiB respectively. Golden snapshot forks inherit their source site;
image creation uses `Site`.

Production control-plane endpoints require HTTPS. HTTP is accepted only on
loopback for test servers. Redirects are rejected so bearer credentials cannot
be sent to another host.

## Credentials

After an existing `devbox login` or `nsc login`, the official SDK can load the
shared user session. On macOS that file is
`~/Library/Application Support/ns/token.json`; other platforms use
`os.UserConfigDir()/ns/token.json`. A session is exchanged for a short-lived
tenant token by the SDK. The session file is not a raw bearer token.

For unattended operation, prefer a workload identity or an explicitly scoped
Namespace token. `auth.LoadDefaults()` checks `NSC_TOKEN_FILE`, then
`/var/run/nsc/token.json`, then the user credential. Secrets stay outside
configuration committed to this repository. The adapter neither prints API
response bodies on errors nor writes returned SSH private keys to disk.

## Images and snapshots

A creation request selects one of two template types:

- `Template.Image`: a registered image name or OCI reference containing the
  prepared T3 server. The adapter calls `DevBoxService.Create`, starts the
  machine, and explicitly disables any inherited repository checkout.
- `Template.Source` plus optional `Template.Snapshot`: the golden Devbox name
  and snapshot ID. The adapter calls `CreateFromDevbox`. With no snapshot ID,
  Namespace selects the source's latest completed snapshot.

Do not include a T3 identity, provider tokens, Claude/Codex sessions, private
SSH keys, or repository credentials in a golden image or snapshot. Initialize
those separately for each created machine.

`ListSnapshots` reports provider-created snapshots. A snapshot is ready only
when `completedSnapshot` is present and `abandonedAt` is absent. The inspected
Namespace Devbox API exposes list, restore, and fork, but no explicit
"capture snapshot now" RPC. Consequently this adapter does **not** implement
`provider.Snapshotter`. A capture-on-stop workflow needs live confirmation
before an operator treats it as a guarantee.

## Lifecycle and retry behavior

The adapter maps lifecycle operations to `Fetch`, `Activate`, `Stop`, and
`Expire`. Activation sets `waitForReadiness:true`. Readiness here means the
Namespace instance is available; the caller must separately check T3's
`/.well-known/t3/environment` descriptor and protocol/version compatibility.

Namespace has no idempotency-key field on these creation requests. t3fleet
therefore uses a stable machine name and records `t3fleet-operation-id` as
a label. A repeated creation reconciles that name and only adopts a Devbox
with the same operation label. A name owned by another operation fails.
Namespace label names must match `^[a-z]([a-z0-9-.]*[a-z0-9])?$` and fit in
63 bytes. Values fit in 255 bytes. Kubernetes-style names containing `/` are
not valid; the adapter checks these limits before contacting the API.
Creation requests are never blindly retried. If a request's response is lost,
retry with the same name and operation ID to reconcile the result. A
concurrent collision can still return an API error; this does not justify
creating another name automatically.

`Stop` and `Delete` resolve the machine's current name before calling `Stop`
and `Expire`. After `Expire`, `Delete` polls `Fetch` until the machine is not
found or has a terminal `destroyedAt` state. A successful request alone does
not establish completed cleanup. The wait honors cancellation and is bounded
to two minutes; on failure the caller must retain its pending cleanup record.
Deletion must be an explicit operator action.

## Private T3 transport

Activation with `includeSshCredentials:true` supplies a transient SSH key,
instance ID, user, and ingress domain. The official Go ingress helper opens:

```text
wss://gate.<ingressDomain>/<instanceId>/hsvc.unixsocket?name=ssh
```

The WebSocket uses a Namespace bearer token and verified TLS. Native Go SSH
runs inside that connection. As in Namespace's maintained Devbox SDK, the
TLS-authenticated gateway and exact instance route establish server identity;
there is no independent SSH host-key pin. This trust assumption is specific
to this provider gateway, not a policy for arbitrary SSH hosts.

`Connect` returns a private dialer bound to one loopback port on that machine.
It does not publish an unauthenticated HTTP URL. The caller must invoke
`Endpoint.Close` when retiring the transport. Canceling the short setup
context after `Connect` succeeds does not close the returned endpoint. Each
subsequent dial takes its own context. Setup has a maximum 90-second deadline.

`Exec` opens an SSH session, quotes argument and environment data, returns
stdout/stderr and exit code, and closes its connection. Canceling its context
closes the session. Output is bounded to 8 MiB per stream.

## Sources and validation

The contract was checked against the official
[Go integrations SDK](https://github.com/namespacelabs/integrations) at commit
`3b5e484d4fbf605f45f52e01c70f61f123b91ad5` and the maintained TypeScript SDK
`@namespacelabs/sdk@1.6.0`. The generated Go Devbox package uses the
`namespace.private.devbox.v1beta` protobuf name; the maintained TypeScript SDK
uses the same service. This naming does not imply that the stable Compute API
and the Devbox API are interchangeable.

- [Public API and authentication](https://namespace.so/docs/reference/api)
- [Devbox lifecycle](https://namespace.so/docs/reference/typescript-sdk/devbox/manage)
- [Devbox images](https://namespace.so/docs/devbox/images)
- [Namespace SDKs](https://namespace.so/docs/reference/sdk)

Offline tests cover the HTTP request contract, operation reconciliation,
snapshot selection, stop/start/delete, cancellation, credential redirect
rejection, and private connection ownership. They do not establish current
cloud account permissions, snapshot capture timing, boot behavior, or a live
T3 pairing. Report those separately after a real cloud canary.

# Adding a provider

Cloud backends implement the public `pkg/provider.Provider` interface. The fleet manager owns persistence, startup readiness, stable local ports, and T3 pairing. An adapter owns only vendor lifecycle and private connectivity.

## 1. Establish the native contract

Use the provider's maintained API or SDK. Record the endpoint, authentication flow, version, and lifecycle semantics in `docs/providers-NAME.md`. Do not start by wrapping a CLI whose output or `PATH` becomes an implicit runtime contract.

Prove these behaviors before advertising support:

- Create from a prepared template and inspect the resulting machine.
- Resume the same persistent machine, stop it, and delete it explicitly.
- Execute the preinstalled runtime hook with bounded output and cancellation.
- Reach a private loopback port without publishing an unauthenticated server.
- Reconcile a creation after a timeout without producing another billable VM.

Provider startup is not T3 readiness. The manager calls `t3fleet-runtime-info`, compares the T3 version, and checks the descriptor through the returned endpoint.

## 2. Implement the required interface

Create `pkg/provider/NAME`. Keep vendor response types inside that package and return the common domain types:

```go
var _ provider.Provider = (*Client)(nil)
```

| Method | Contract |
| --- | --- |
| `Name` | Stable driver type, such as `boat`. Several named instances may have the same type. |
| `Create` | Use `OperationID` to deduplicate or safely reconcile a request. Never change names automatically after a lost response. |
| `Get` | Map vendor states to the common state set; return `provider.ErrNotFound` for absence. |
| `Start` | Start/resume the existing machine, honoring context cancellation. |
| `Stop` | Stop the existing machine; preserve its persistent disk according to documented provider semantics. |
| `Delete` | Delete only the specified machine and wait for confirmation where the API is asynchronous. |
| `Exec` | Preserve argv/environment values, bound output, return the exit code, and stop work when canceled. |
| `Connect` | Return a private endpoint for the requested machine port and a cleanup function for owned resources. |

For ordered dispatch, return `provider.ErrCreateRejected` only when the adapter
can confirm that the attempt allocated no machine. A transport failure, an
ambiguous response, or a failure after allocation must retain the original
provider so that the fleet can reconcile it.

Do not log `ExecResult`, `Endpoint.URL`, endpoint headers, or private keys. These can contain credentials.

`Endpoint.DialContext` lets an adapter provide a private transport, such as Namespace's authenticated SSH tunnel. A successful endpoint must outlive its connection-setup context; the proxy calls `Endpoint.Close` when the request or WebSocket ends. Subsequent dials receive their own context.

HTTP endpoints may include provider query credentials. Those credentials must stay inside the daemon and survive every upstream path/query rewrite. Refuse credential-bearing redirects to another origin.

## 3. Add optional capabilities honestly

Implement `provider.StateObserver` if the adapter can read the current states
of managed remote IDs without waking their machines. Use the configured account,
honor cancellation, and return only display-safe states for the requested IDs.
An omitted ID is unobserved, not proof of deletion.

Implement `provider.Snapshotter` only if the provider offers a reliable explicit capture operation:

```go
var _ provider.Snapshotter = (*Client)(nil)
```

The returned snapshot must be deployable and ready. If capture is asynchronous, wait for completion or fail on a terminal error. Do not equate a snapshot request being accepted with a usable snapshot.

A backend can support creating from existing snapshots without implementing explicit capture. Namespace is an example. Add a separate optional interface only when a real second implementation or caller needs it; do not expand the required interface for a hypothetical feature.

## 4. Wire configuration at the application layer

Add the provider's non-secret configuration and credential resolution to `internal/config`. Instances declare a driver with `providers.<name>.type`; they own their credentials, size and template. Register the new type in configuration decoding, validation and construction. A new adapter therefore needs a small configuration change, but no change to fleet lifecycle or the T3 proxy.

Keep account selection explicit. Implement `provider.ScopeIdentifier` with a stable identifier derived from the API endpoint and account, independent of credential rotation. Bind each request to that account and resolve credentials only when the instance is used. An unavailable fallback must not prevent the controller from starting.

If the adapter has placement defaults such as a site or injected environment, implement `provider.PlacementProvider`. Honor the saved `CreateRequest.Placement` on retry, including an explicitly empty setting. A config edit must not redirect an existing reservation.

Templates use `provider.Template`:

- `Image` is a registered or OCI image reference.
- `Snapshot` is a provider snapshot reference.
- `Source` identifies a golden source machine when snapshots are scoped to it.

Document unsupported combinations and validate them before a billable request.

## 5. Test the seam

Use `httptest` and injectable transports, clocks, or dialers to exercise the actual request contract. At minimum, cover:

- Successful lifecycle and exact vendor state mapping.
- Creation reconciliation and a lost-response retry with the same operation ID.
- Cancellation during readiness polling and connection setup.
- Failure responses, malformed responses, and absent machines.
- Argument quoting if the vendor only accepts shell commands.
- Endpoint ownership and cleanup after a long-lived connection.
- Credential redaction, redirect refusal, and negative authorization cases.

Run the repository checks:

```sh
make fmt
make lint
make test
make build
```

Offline tests must not consume cloud credentials or create resources. Run a separate, explicitly scoped cloud canary on a dedicated machine. Record the image/snapshot reference, versions, lifecycle results, and final stopped/deleted state. Never commit tokens, private URLs, or raw command output containing secrets.

## 6. Document support and limits

Update the README, provider guide, example configuration, and any capability comparison. Separate mock-test proof, real API reads, real machine lifecycle, and an actual T3 pairing. A cloud machine that boots is not yet an end-to-end T3 integration.

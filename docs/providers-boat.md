# Boat provider

The Boat adapter uses the v1 HTTP API directly. It does not spawn the Boat CLI.
The API contract was checked against the official OpenAPI schema on 2026-11-18.

## Authentication and account scope

Supply an API key and an explicit organization to `boat.New`. Credentials stay in
memory and are sent as an `Authorization: Bearer` header. The organization is sent
as `X-Boat-Org`. If the organization is omitted, Boat uses the account's active
wallet. The adapter never changes that wallet.

The production base URL is `https://boat.dev/api/v1`. A custom base URL must use
HTTPS, except for loopback HTTP servers used by tests. Redirects are refused so
the API bearer token cannot be forwarded to a different server.

The adapter does not load the CLI's credentials itself. The daemon's configuration
layer owns credential loading and its file permissions.

New sandboxes use `noEnv: true` by default. This prevents implicit injection of
the Boat account's repositories, secrets, and agent credentials. Setting
`Config.Environment` explicitly opts in to the named Boat environment instead.
Use credential-free, versioned golden snapshots independently of this option.

## Lifecycle

| t3fleet operation | Boat API |
| --- | --- |
| Create from template | `POST /sandboxes`, body `from` is a saved snapshot name |
| Inspect | `GET /sandboxes/{id}` |
| Start | `POST /sandboxes/{id}/resume`, then poll inspection |
| Stop | `POST /sandboxes/{id}/stop`, then poll inspection |
| Delete | `DELETE /sandboxes/{id}`, then poll deletion operation |
| Execute | `POST /sandboxes/{id}/commands` |
| Private T3 endpoint | `POST /sandboxes/{id}/host` |
| Save template | `POST /saved-snapshots`, then poll saved snapshot |

Create requires a non-empty operation ID. It becomes Boat's `Idempotency-Key`,
which deduplicates create retries for 24 hours. The caller must persist and reuse
the same operation ID after a lost response. The adapter also owns the durable
`t3fleet-id` label: it sets that label to the operation ID and searches all pages
of the label-filtered sandbox list **before** posting a create. A unique match
is reused even after the HTTP idempotency window expires. Multiple matches,
incomplete metadata, lookup failures, and malformed pagination fail closed rather
than creating another billable machine. The adapter does not automatically retry
creates or commands.

Boat reports a stopped sandbox as `archived`, while its CLI displays `stopped`.
`ready`, `idle`, and `running` all map to a running machine. Starting and stopping
wait until the provider reports the requested final state. Every wait honors the
caller's context.

Delete sends `X-Ascii-Confirm-Delete` with the exact sandbox ID. It waits for the
returned deletion operation to complete. A sandbox that is already absent counts
as deleted. Deleting a sandbox is irreversible; saved snapshots are separate
resources and remain available.

## Snapshots and readiness

`Template.Snapshot` is the saved snapshot name accepted by `Create.from`.
`Template.Image` may carry the same name for callers that use a common template
field across providers. Distinct values in both fields are rejected.

A snapshot save first returns `saving`. The adapter polls
`GET /saved-snapshots/{name}` until `ready` or `failed`. A returned
`provider.Snapshot.ID` is the deployable saved snapshot **name**, rather than the
immutable storage artifact ID. Reusing a name replaces the artifact behind that
name, so golden template publishing should use versioned names.

Provider readiness does not prove T3 readiness. The manager must probe the T3
descriptor and validate its version after the sandbox is running. Restoring a
snapshot does not reinstall T3; boot services in the prepared image must start it.

Boat's default auto-stop policy applies because the create request does not
override `ttlSeconds`. Keep this provider policy separate from any future
agent-aware t3fleet idle policy.

## Private transport

The adapter hosts the T3 port with `public: false` and `localhost: true`. T3 can
therefore bind only to loopback inside the sandbox. Boat returns an HTTPS URL
with a `_token` query parameter. The adapter rejects ungated, non-HTTPS, and
credential-in-userinfo URLs.

The token-bearing URL is an access-grant bootstrap, not a URL to attach unchanged
to every proxied request. The adapter requests the public T3 descriptor with that
token and a fresh cookie jar, follows at most five HTTPS redirects within exactly
the same origin, and requires a cookie valid for the endpoint. Boat's live grant
flow sets `_port_auth` and redirects to the token-free URL. The adapter does not
depend on that cookie name, but rejects missing, expired, wrongly scoped, or
denied grants.

The resulting `Endpoint.URL` has `_token` removed. `Endpoint.Headers.Cookie`
contains the private grant for subsequent HTTP and WebSocket requests. Both are
private connection data: the proxy must bind the cookie to this endpoint's origin
and must never log or expose it to downstream clients. A new connection uses a
new cookie jar, so one sandbox cannot inherit another sandbox's grant. Neither
the account API bearer token nor a T3 bearer token is sent during this bootstrap.
The caller attaches T3's separate service authentication after connecting.

`Config.AccessHTTPClient` optionally customizes the hosted-port transport, for
example for a trusted TLS test server. The adapter still owns the fresh cookie
jar, redirect limit, and same-origin policy. Production uses the standard HTTPS
transport. Application readiness is checked separately after the grant exists.

## Commands and errors

Boat accepts shell text, so the adapter encodes an argv request using POSIX
single-quote escaping, validates environment names, and rejects NUL bytes. A
completed command with a nonzero exit code returns that code to the caller.
The current synchronous execution timeout is 60 seconds.

Boat retains command input and output in command history for 90 days. Therefore
**never put credentials in command arguments or environment values**: they are
part of the retained command input. In particular, t3fleet's runtime-info command
is static; its T3 bearer token is produced as output rather than passed in argv.

The adapter transports stdout and stderr through private files rather than
command history. Each execution creates a cryptographically random directory
under `/tmp/t3fleet-exec-`, with mode `0700`, and redirects both streams to mode
`0600` files. The command API receives only a numeric exit status. The adapter
then reads both files through the authenticated files API with `encoding=utf8`.
It validates response metadata and limits each stream to 4 MiB. Malformed status,
unexpected encoding, missing metadata, size mismatch, and oversized output fail
closed without returning partial output.

The files API reports paths relative to `/home/user`, even when the request uses
an absolute path: `/tmp/output` is returned as `../../tmp/output`. The adapter
resolves that relative representation using POSIX path rules and still requires
an exact canonical match with the requested private output file. This behavior
was checked read-only against a nonsecret marker on 2026-11-18.

The temporary directory is deleted through the native recursive files API on
success or failure, including cancellation. Cleanup has its own bounded context
and does not retry execution. A cleanup failure returns an error and discards
retrieved outputs. This avoids storing secrets in Boat's command-history stdout
or stderr; it does not make secret command arguments safe or claim that the
provider cannot access sandbox files.

Errors expose only HTTP status and a validated error code. Vendor messages and
response bodies are excluded because they may contain credentials, command
output, or private URLs. A 404 maps to `provider.ErrNotFound`.

Execution failures additionally implement the exported `*boat.ExecError` type.
Its safe diagnostic identifies a static phase (`execute`, `completion`,
`stdout-read`, `stderr-read`, or `cleanup`) and a static validation reason.
Transport errors may add an HTTP status and an allowlisted public lifecycle code.
The diagnostic never interpolates command output, private filenames, URLs,
arbitrary causes, or arbitrary vendor messages. The underlying cause remains
available through `Unwrap` for cancellation checks; do not log that raw cause.

## Validation

Mock HTTP tests cover lifecycle polling, snapshot readiness/failure,
idempotency headers, deletion confirmation, cancellation, shell argument
escaping, private endpoint handling, redirect refusal, and error redaction.
Execution tests run the generated shell text against harmless local fixtures,
verify private file permissions, keep secret markers out of the mock command
history, and exercise file validation, nonzero exits, cancellation, and cleanup
failures.
TLS grant tests cover same-origin cookie redirects, token removal, account-header
isolation, cross-origin redirect rejection, malformed grants, redirect limits,
and context cancellation. A native API scratch-machine probe also verified the
cookie grant without printing cookie values or token-bearing URLs.
They do not create cloud resources.

Official references:

- [HTTP API overview](https://docs.boat.dev/api/v1)
- [OpenAPI schema](https://docs.boat.dev/openapi/boat-v1.yaml)
- [API keys](https://docs.boat.dev/api-keys)
- [Snapshot templates](https://docs.boat.dev/platform-guide/templates-and-sizing)

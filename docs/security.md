# Security model

`t3fleet` can create billable machines, run commands there, and proxy an authenticated T3 server. Run it as your regular user on a trusted computer. This early release is designed for a single operator and loopback connections, not as an Internet-facing multi-tenant service.

## Network and authorization

The control API and per-machine T3 listeners bind loopback. The control API requires a random admin bearer token. It refuses browser Origin headers and non-loopback Host values. T3 proxy routes use pairing-derived bearer tokens and short-lived, scoped WebSocket tickets. The proxy checks its configured origin and the allowed T3 browser origin.

The public descriptor is part of T3 discovery. It is served from persisted metadata and never wakes a machine. Authenticated requests for live data can wake it; chat data is not cached for offline use. Do not treat loopback as a barrier against a hostile process running under the same user.

The proxy uses a separate per-machine T3 service token upstream. A downstream client does not receive that token or the provider's private endpoint credentials. Redirects and error handling are constrained so credentials do not escape through a vendor error message or a new origin.

## Storage

The daemon's state directory is `0700`; state, lock, and admin-token files are private. State writes are atomic and synchronized. The store rejects unsafe final paths and uses an exclusive process lock. A second daemon must not bypass the lock.

Private state contains service tokens and T3 client authorizations. File permissions are not encryption at rest: an account or machine administrator who can read the files can use those credentials. Protect backups accordingly. Do not attach the state file to issues or support bundles.

The caller owns the trust of parent directories. Do not place state under a shared or attacker-controlled parent directory.

## Cloud transport

- **Boat:** the T3 server remains loopback-bound on the VM. Boat supplies a token-gated HTTPS URL. The URL is itself a secret and must not enter logs or public configuration.
- **Namespace:** the adapter uses the official authenticated TLS gateway and native Go SSH, then tunnels to the T3 loopback port. Its SSH trust follows the provider SDK: the authenticated exact-instance gateway route is the trust anchor, rather than an independently pinned host key. That assumption applies only to this provider gateway.

Each named instance pins a Boat organization or Namespace tenant ID. Changing a login cannot silently move an existing reservation to a new account. Cloud API keys and session credentials remain in the daemon's runtime credential source. Returned Namespace SSH keys are kept in memory, not written to disk. Use scoped/workload credentials for unattended installations when the provider supports them.

Boat creates machines with `noEnv: true` by default. Setting `providers.<name>.environment` explicitly opts into the named Boat environment, including any repositories, credentials, or secrets that it injects.

Boat retains command inputs, including argv and environment, in its command history for 90 days. Never put credentials in those inputs. The adapter redirects stdout and stderr into private remote files (`0700` directory, `0600` files) and retrieves them through the files API so the runtime hook's service token does not enter command-output history. It attempts cleanup afterward; cancellation or an unknown remote execution outcome can leave files behind. Do not assume cleanup always succeeds.

## Templates, snapshots, and model logins

A reusable golden image must contain only software and startup configuration. It must not contain:

- A T3 identity/database, upstream service token, or downstream client token.
- A Claude/Codex session or model API key.
- Cloud credentials, private SSH keys, private projects, or `.env` files.

T3 state and its service token are created independently on each machine after boot. Connect model accounts after creating that machine. The authentication and billing rules of the chosen model provider still apply.

An explicit snapshot of a used machine is a **private backup**. It can retain code and credentials. Do not publish it as a golden template or distribute it to other users. Deleting a VM does not necessarily delete its saved snapshots.

## Operational limits

Stopping the daemon only closes local connections. It does not stop agents, remove VMs, or guarantee that billing ends. Explicitly stop or delete cloud resources and verify the provider result.

The current gateway has no multi-user authorization model, hosted TLS management, remote idle watchdog, or complete offline protocol emulation. The fleet exposes scoped [native linked-environment operations](dispatch.md#launch-from-another-t3-server); URL-only attachment credentials are not relayed. See [protocol scope](protocol.md#current-scope).

To reach an always-on host, use authenticated SSH forwarding or configure `dispatch.externalOrigin` behind a TLS reverse proxy restricted to a private network. Keep the administration port private. The HTTPS route uses the same pairings and authorization checks as the local route; it is not a public hosting mode.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting for this repository if it is available. Otherwise, open an issue asking the maintainer for a private contact channel without including exploitation details, tokens, private URLs, or user data. Do not post live credentials in a public issue.

## Recovery operations

The `admin` command group acts on individual managed workers. Multi-delete snapshots the selected IDs and validates them before deletion. A script must provide an explicit `--ids` list and `--yes`. Worker identity and service credentials must be unique even when workers share a template. Never share a snapshot taken from a used worker as a golden template.

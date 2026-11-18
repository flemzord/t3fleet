# t3fleet

Run T3 Code agents on persistent **Boat** and **Namespace** machines from one local Go daemon.

`t3fleet` creates machines from prepared images or snapshots, waits for their T3 server, and gives T3 Code a stable, authenticated local endpoint. Provider adapters use native APIs; the Boat and Namespace CLIs are not needed while the daemon runs.

**Status: early alpha.** The CLI, lifecycle, private state, and live HTTP/WebSocket gateway are implemented. This is a loopback-only first release, with explicit stop/start controls. An optional agent-aware idle timeout is available; offline chat replay is not implemented. Read the [current limits](docs/roadmap.md) before using it for unattended work.

## How it fits together

```text
T3 Code desktop
      │ HTTP / WebSocket, local pairing token
      ▼
t3fleet daemon on your computer
      │ provider API + private authenticated connection
      ├── Boat sandbox ─────── T3 server → Claude Code / Codex
      └── Namespace Devbox ─── T3 server → Claude Code / Codex
```

The daemon owns cloud lifecycle and connection state. T3 Code remains the user interface. Coding agents run on the remote machine and contact their model provider directly. You connect Claude/Codex on each machine; your local login is not copied into an image.

Keep the daemon running while using a machine through its local endpoint. Stopping the daemon does **not** stop or delete cloud machines, which may remain billable.

## First run

You need:

- T3 Code desktop and a matching T3 server version in a prepared Linux template.
- A Boat account, a Namespace account, or both, with permission to create machines.
- A credential-free template containing T3 and the [t3fleet runtime hook](docs/getting-started.md#2-prepare-a-template). Self-contained Nix inputs and a [reference image recipe](images/README.md) are included.
- Go 1.27 with a working C compiler for race tests, or Nix.

Build from source:

```sh
git clone https://github.com/flemzord/t3fleet.git
cd t3fleet
nix develop
make lint
make test
make build
```

Without Nix, run the same `make` commands with Go 1.27 installed. The Nix shell selects Go 1.27.3 through Go's verified toolchain download (network access is needed on first use); CI also checks the minimum Go version declared in `go.mod`.

Copy and edit the example configuration:

```sh
mkdir -p "$HOME/.config/t3fleet"
cp examples/config.json "$HOME/.config/t3fleet/config.json"
```

Set `stateDir` to an absolute private path. Replace the template placeholders with your prepared snapshot/image references. Remove any provider you will not use. Set the Boat `organization` explicitly to choose the billable wallet. Boat environment injection is disabled by default; set `providers.boat.environment` only to opt into a named environment and its contents. The file contains references to credentials, not the credentials themselves.

Start the daemon in one terminal:

```sh
./bin/t3fleet serve --config "$HOME/.config/t3fleet/config.json"
```

In a second terminal, using the same configuration:

```sh
./bin/t3fleet create --name my-agent --provider boat --config "$HOME/.config/t3fleet/config.json"
./bin/t3fleet list --config "$HOME/.config/t3fleet/config.json"
```

`create` displays an activity spinner and elapsed time while it waits for the machine and T3 to become ready, then prepares the association and opens T3 Code automatically. Confirm the backend in T3 to finish saving it. You do not need a separate `pair` command or to copy a link. T3 requires this confirmation for external pairing links; launching the app alone does not prove the connection was saved.

Progress appears on standard error, leaving standard output as machine JSON. When output is redirected or `TERM=dumb`, progress uses plain start/completion messages instead of animation.

New machine names receive the `t3fleet-` prefix automatically, without doubling
an existing prefix. `--name my-agent` creates `t3fleet-my-agent`. Existing machines
and saved batches retain their original names. The final name must fit 32 characters.

The returned JSON contains the machine `id` for subsequent commands. On a headless host or in scripts, use `create --no-register` to skip desktop association. Use `pair MACHINE_ID` later to print a short-lived, one-use `t3code://` link. Do not share that link or put it in logs.

If T3 cannot be opened, the machine remains created and the CLI reports its ID. Run `pair` for that ID with the same configuration to recover; do not create another VM.

For Namespace, use `--provider namespace`. [The complete setup guide](docs/getting-started.md) explains credentials, template preparation, model sign-in, and troubleshooting.

To pair T3 with one endpoint that creates a Boat worker on the first message,
use [the on-demand config example](examples/dispatch-boat.json) and the
[dispatch guide](docs/dispatch-poc.md). The regular [provider config](examples/config.json)
supports Boat and Namespace, including a mix of preprovisioned workers in the
fixed-pool dispatch mode. On-demand dispatch currently provisions only Boat.

## Stop idle machines automatically

Add `"idleTimeout": "30m"` to your configuration and restart `serve` with the
new binary. The daemon stops a VM only after its idle window and fresh T3 checks
show no agent run, subagent, pending approval or background task. Failed or
unknown activity checks keep it running. Useful authenticated requests wake the
same VM with its existing T3 pairing.

This is disabled by default (`"0"`). Known background RPC subscriptions and
pings do not keep a VM awake or wake it again. The policy needs `serve` running;
it does not protect detached non-T3 jobs or schedule future wakes. Read
[idle lifecycle and safety limits](docs/idle-lifecycle.md) before enabling it.

## Create and delete in bulk

```sh
./bin/t3fleet create --name workers --provider boat --count 10 --concurrency 3
./bin/t3fleet create --resume BATCH_ID --no-register
./bin/t3fleet delete
# Explicit scriptable selection:
./bin/t3fleet delete --ids MACHINE_ID_1,MACHINE_ID_2 --yes
```

Bulk create uses `t3fleet-workers-01` through `t3fleet-workers-10` with the same template and
size. It saves the full plan before allocation, reports each outcome and retains
failed members for safe recovery. Use `--no-register` for scripts. Interactive
association opens each ready VM in T3 and waits before the next handoff; T3 still
requires confirmation for every backend. `delete` lists only managed machines,
accepts multiple row numbers and confirms the exact selection before deletion.
Read [bulk lifecycle and recovery](docs/bulk-lifecycle.md) before running a batch.

## Everyday commands

All commands take `--config PATH`. The CLI sends authenticated requests to the local daemon; it does not modify the state file directly.

| Command | Result |
| --- | --- |
| `create --name NAME --provider boat` | Create from the configured Boat template, wait for readiness, and open T3 association. |
| `create --name NAME --provider namespace` | Create from the configured Namespace template, wait for readiness, and open T3 association. |
| `create … --no-register` | Create without opening T3; useful for headless hosts and scripts. |
| `list` / `inspect ID` | Read the daemon's managed records. |
| `pair ID` | Ensure readiness and print a T3 Code pairing link. |
| `pair-dispatch` | Print a T3 Code pairing link for the configured virtual endpoint. |
| `stop ID` / `start ID` | Explicitly stop or resume a machine. Authenticated proxy traffic can also wake it. |
| `snapshot ID --name NAME` | Create a private backup where the provider supports explicit capture. |
| `create … --count 10` | Create an identical batch with bounded concurrency and a durable recovery ID. |
| `create --resume BATCH_ID` | Resume incomplete batch members and reopen ready members for association. |
| `delete` | Select managed machines interactively and confirm permanent deletion. |
| `delete --ids ID,ID --yes` | Delete exactly the listed managed IDs in scripts. |
| `delete ID` | Permanently delete the remote machine and then its local record. |

A snapshot of a used machine can contain code, credentials, and sessions. Treat it as a private backup, not as a shared golden template. Namespace currently supports creating from existing snapshots but does not implement the explicit `snapshot` command. See the [Boat](docs/providers-boat.md) and [Namespace](docs/providers-namespace.md) adapter notes.

## Documentation

- [Getting started and troubleshooting](docs/getting-started.md)
- [Validation evidence and limits](docs/validation.md)
- [Automatic idle stop and wake](docs/idle-lifecycle.md)
- [Bulk lifecycle and recovery](docs/bulk-lifecycle.md)
- [Architecture and lifecycle](docs/architecture.md)
- [T3 protocol gateway](docs/protocol.md)
- [Experimental single-environment dispatch](docs/dispatch-poc.md)
- [Adding a provider](docs/adding-a-provider.md)
- [Security and credential handling](docs/security.md)
- [Current scope and roadmap](docs/roadmap.md)
- [Contributing](CONTRIBUTING.md)

## Development

```sh
make fmt    # format Go source
make lint   # require gofmt-clean source and run go vet
make test   # run all tests with the race detector
make build  # write bin/t3fleet
```

CI runs the same lint, test, and build commands on Linux and macOS, plus the Nix runtime contract on Linux. Offline tests do not need cloud credentials or create billable resources. Optional genuine-T3 and paid cloud validation commands are documented in [Contributing](CONTRIBUTING.md#optional-live-validation).

## License and inspiration

MIT; see [LICENSE](LICENSE). The protocol design was studied from [t3boat](https://github.com/paukraft/t3boat), by Pau Kraft. t3fleet is an independent project and is not affiliated with T3 Code, Boat, or Namespace. See [third-party notices](THIRD_PARTY_NOTICES.md).

# t3fleet

Run T3 Code agents on persistent **Boat** and **Namespace** machines from one local Go daemon.

`t3fleet` creates machines from prepared images or snapshots, waits for their T3 server, and gives T3 Code a stable, authenticated local endpoint. Provider adapters use native APIs; the Boat and Namespace CLIs are not needed while the daemon runs.

**Status: early alpha.** The CLI, lifecycle, private state, and live HTTP/WebSocket gateway are implemented. This is a loopback-only first release, with explicit stop/start controls. It does not yet reproduce t3boat's offline chat cache or agent-aware automatic sleep. Read the [current limits](docs/roadmap.md) before using it for unattended work.

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

Without Nix, run the same `make` commands with Go 1.27 installed. Nix pins Go 1.27.3; CI also checks the minimum Go version declared in `go.mod`.

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

Use the returned `id` for subsequent commands:

```sh
./bin/t3fleet pair MACHINE_ID --config "$HOME/.config/t3fleet/config.json"
```

Open the printed `t3code://` link on the computer running T3 Code, then accept the association. The link contains a short-lived, one-use pairing credential; do not share it or put it in logs.

For Namespace, use `--provider namespace`. [The complete setup guide](docs/getting-started.md) explains credentials, template preparation, model sign-in, and troubleshooting.

## Everyday commands

All commands take `--config PATH`. The CLI sends authenticated requests to the local daemon; it does not modify the state file directly.

| Command | Result |
| --- | --- |
| `create --name NAME --provider boat` | Create from the configured Boat template and wait for T3 readiness. |
| `create --name NAME --provider namespace` | Create from the configured Namespace template and wait for T3 readiness. |
| `list` / `inspect ID` | Read the daemon's managed records. |
| `pair ID` | Ensure readiness and print a T3 Code pairing link. |
| `stop ID` / `start ID` | Explicitly stop or resume a machine. Authenticated proxy traffic can also wake it. |
| `snapshot ID --name NAME` | Create a private backup where the provider supports explicit capture. |
| `delete ID` | Permanently delete the remote machine and then its local record. |

A snapshot of a used machine can contain code, credentials, and sessions. Treat it as a private backup, not as a shared golden template. Namespace currently supports creating from existing snapshots but does not implement the explicit `snapshot` command. See the [Boat](docs/providers-boat.md) and [Namespace](docs/providers-namespace.md) adapter notes.

## Documentation

- [Getting started and troubleshooting](docs/getting-started.md)
- [Architecture and lifecycle](docs/architecture.md)
- [T3 protocol gateway](docs/protocol.md)
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

CI runs the same lint, test, and build commands on Linux and macOS. Offline tests do not need cloud credentials or create billable resources. Optional genuine-T3 and paid cloud validation commands are documented in [Contributing](CONTRIBUTING.md#optional-live-validation).

## License and inspiration

MIT; see [LICENSE](LICENSE). The protocol design was studied from [t3boat](https://github.com/paukraft/t3boat), by Pau Kraft. t3fleet is an independent project and is not affiliated with T3 Code, Boat, or Namespace. See [third-party notices](THIRD_PARTY_NOTICES.md).

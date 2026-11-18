# t3fleet

Run T3 Code on persistent [Boat](https://boat.dev/) sandboxes and [Namespace](https://namespace.so/) Devboxes from a local Go daemon.

Create a remote machine from a prepared template, pair it with T3 Code Desktop, and resume it later through the same local endpoint. Claude Code or Codex runs on the remote machine. t3fleet handles provisioning, power controls, and the private connection through the provider APIs.

**Early alpha.** t3fleet listens on loopback and needs a running daemon to relay traffic. Check the [validated workflows](https://github.com/flemzord/t3fleet/blob/main/docs/validation.md) and [current limits](https://github.com/flemzord/t3fleet/blob/main/docs/roadmap.md) before using it for unattended work. Cloud machines and snapshots can incur charges; stopping the daemon does not stop or delete them.

## Install

Download the archive for macOS or Linux (amd64 or arm64) from [GitHub Releases](https://github.com/flemzord/t3fleet/releases). Compare the archive's SHA-256 hash with `checksums.txt`, then extract it. The archive contains the `t3fleet` binary and example configuration files.

To build from source with Go 1.27 or newer:

```sh
git clone https://github.com/flemzord/t3fleet.git
cd t3fleet
make build
```

The source build writes `bin/t3fleet`. Run `nix develop` first if you want the pinned development toolchain.

## Create your first machine

You need a provider account with permission to create machines, and a [prepared Linux template](https://github.com/flemzord/t3fleet/blob/main/docs/getting-started.md#2-prepare-a-template) with the t3fleet runtime hook. Use matching T3 Desktop and server versions. The sample config targets [T3 Code 0.1.11 nightly (2026-11-17)](https://github.com/pingdotgg/t3code/releases/tag/v0.1.11-nightly.20261117.4838).

1. Create the configuration. These examples use the extracted binary; for a source build, use `./bin/t3fleet` instead.

   ```sh
   ./t3fleet init
   ```

   `init` asks for the provider, the Boat organization to bill, the prepared template, and an optional default VM size for each selected provider. It writes a private file at the default location (`t3fleet status` shows it). For scripts, pass the values as flags, such as `init --provider boat --boat-organization ORG --boat-snapshot SNAPSHOT --boat-size small --no-input`. The file holds no secrets. For Boat, export `BOAT_API_KEY` in the daemon's shell. For Namespace, use the [Namespace credential setup](https://github.com/flemzord/t3fleet/blob/main/docs/providers-namespace.md). See [examples/config.json](examples/config.json) for a sample configuration.

2. Start the daemon:

   ```sh
   ./t3fleet serve
   ```

3. Check it from a second terminal:

   ```sh
   ./t3fleet status
   ```

4. Create a machine. `--provider` is only needed when both providers are configured.

   ```sh
   ./t3fleet create --name my-agent
   ```

5. Confirm the association in T3 Code Desktop when it opens. Sign in to Claude Code or Codex on the remote machine using your model vendor's flow.

Use `create --size` to override the [configured VM size](docs/getting-started.md#vm-size) for one machine or batch.

For a headless host, use `create --no-register`, then `pair my-agent` to get a short-lived pairing link. If Desktop does not open, run `open my-agent` to retry. Keep the pairing link private.

Run `./t3fleet --help` for commands and `./t3fleet COMMAND --help` for options and examples. Use `list` to see managed machines, `status` to check the daemon, and `start`, `stop`, `inspect` or `delete` to manage them. Commands accept a machine name (with or without the `t3fleet-` prefix), a full ID, or a unique ID prefix; `delete` requires a full ID or exact name. Add `--json` for scripts. For a non-default config path, use `init --config PATH`, then set `T3FLEET_CONFIG` instead of passing `--config` to each command. `./t3fleet completion zsh` (or `bash`, `fish`) prints a shell completion script that completes machine names.

For experimental dispatch, use `dispatch pair` for a Desktop pairing link or `dispatch pair --peer-access LEVEL` for a Linked Environments code.

## Guides

- [Configuration, templates, and troubleshooting](https://github.com/flemzord/t3fleet/blob/main/docs/getting-started.md)
- [Batch creation and recovery](https://github.com/flemzord/t3fleet/blob/main/docs/bulk-lifecycle.md)
- [Optional idle stop and automatic wake](https://github.com/flemzord/t3fleet/blob/main/docs/idle-lifecycle.md)
- [Experimental dispatch: fixed pools and on-demand workers](https://github.com/flemzord/t3fleet/blob/main/docs/dispatch-poc.md)
- [Choose the on-demand provider order and fallbacks](https://github.com/flemzord/t3fleet/blob/main/docs/dispatch-poc.md#fall-back-to-namespace)
- [Launch on-demand threads from another T3 server with Linked Environments](https://github.com/flemzord/t3fleet/blob/main/docs/dispatch-poc.md#launch-from-another-t3-environment)
- [Architecture](https://github.com/flemzord/t3fleet/blob/main/docs/architecture.md) and [adding a provider](https://github.com/flemzord/t3fleet/blob/main/docs/adding-a-provider.md)
- [Contributing](https://github.com/flemzord/t3fleet/blob/main/CONTRIBUTING.md) and [reporting a security issue](https://github.com/flemzord/t3fleet/security/policy)

## License

[MIT](LICENSE). The pairing and gateway design draws on [t3boat](https://github.com/paukraft/t3boat) by Pau Kraft; see [third-party notices](THIRD_PARTY_NOTICES.md). t3fleet is independent of T3 Code, Boat, and Namespace.

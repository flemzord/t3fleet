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

1. Copy the [example config](examples/config.json) from the archive or source tree:

   ```sh
   mkdir -p "$HOME/.config/t3fleet"
   cp examples/config.json "$HOME/.config/t3fleet/config.json"
   ```

2. Set an absolute private `stateDir` and replace the template placeholders. Remove the provider and template entries you do not use. For Boat, set the billable organization and export `BOAT_API_KEY` in the daemon's shell. For Namespace, use the [Namespace credential setup](https://github.com/flemzord/t3fleet/blob/main/docs/providers-namespace.md).

3. Start the daemon. These examples use the extracted binary; for a source build, use `./bin/t3fleet` instead.

   ```sh
   ./t3fleet serve --config "$HOME/.config/t3fleet/config.json"
   ```

4. Create a machine from a second terminal. Change `boat` to `namespace` if you configured Namespace.

   ```sh
   ./t3fleet create --name my-agent --provider boat --config "$HOME/.config/t3fleet/config.json"
   ```

5. Confirm the association in T3 Code Desktop when it opens. Sign in to Claude Code or Codex on the remote machine using your model vendor's flow.

For a headless host, use `create --no-register`, then `pair MACHINE_ID` to get a short-lived pairing link. If Desktop does not open, reuse the returned machine ID with `pair`. Keep the pairing link private.

Run `./t3fleet --help` for commands. Use `list` to find managed machine IDs and `stop`, `start`, or `delete` to manage them. Pass the same `--config` to each command when using a non-default config path.

For experimental dispatch, use `pair-dispatch` for a Desktop pairing link or `pair-dispatch --peer-access LEVEL` for a Linked Environments code.

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

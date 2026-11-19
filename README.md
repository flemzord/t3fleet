# t3fleet

Pair T3 Code with one fleet environment. Add GitHub projects as you work. The first message in each thread starts a dedicated T3 server on [Boat](https://boat.dev/) or [Namespace](https://namespace.so/), clones the selected repository on that worker, and runs the turn there. Follow-ups return to the same worker.

The daemon runs a local T3 controller for pairing and project metadata. You can start the daemon, pair and add projects with zero cloud workers. Your JSON configuration chooses the provider instances, their sizes and templates, the order to try them, and the worker limit.

## Install

With Nix flakes on macOS or Linux (arm64 or amd64):

```sh
nix profile install github:flemzord/t3fleet#t3fleet
```

See [Nix installation and background service](docs/nix.md) for declarative installs and the Home Manager service.

For a tagged version, download the archive for macOS or Linux (amd64 or arm64) from [GitHub Releases](https://github.com/flemzord/t3fleet/releases). Compare the archive's SHA-256 hash with `checksums.txt`, then extract it. Use the examples bundled with that binary: older versions use a different configuration format.

To build from source with Go 1.27 or newer:

```sh
git clone https://github.com/flemzord/t3fleet.git
cd t3fleet
make build
```

The source build writes `bin/t3fleet`. Run `nix develop` first if you want the pinned development toolchain.

## Start a fleet

1. Install the matching T3 executable on the controller host and [prepare a worker template](docs/getting-started.md#prepare-the-workers). The example version is `0.1.11-nightly.20261117.4838`.
2. Run `t3fleet init` or copy the [minimal Boat configuration](examples/boat.json). Set the absolute T3 path, the prepared template, and the Boat organization or Namespace tenant ID. One provider instance is enough; the [multi-provider example](examples/config.json) shows how to add named fallbacks. These entries do not create machines.
3. Run `t3fleet validate --config PATH`, then `t3fleet serve --config PATH`. Keep the service running.
4. In another terminal, run `t3fleet pair --config PATH` and open the link in T3 Desktop. To launch from another T3 server into an existing project, pair it with `t3fleet pair --peer-access approval-required --config PATH`. To register a new project from that server, the link needs `--peer-access full-access` and the caller also needs full access.
5. In Desktop, choose **Add to another machine**, select the fleet, and send a first text message. From a linked T3 server, use the [native project and launch workflow](docs/dispatch.md#launch-from-another-t3-server). Run `t3fleet list --config PATH` to see which provider instance owns the worker.

Use `T3FLEET_CONFIG` to set the config path for all commands. `t3fleet list` and `t3fleet status` show the local fleet state alongside the state reported by [Boat](docs/providers-boat.md#worker-inventory) or [Namespace](docs/providers-namespace.md#worker-inventory). For example, a worker can be locally `ready` while its cloud machine is `stopped`. Checking the inventory does not wake it. Use `t3fleet admin --help` for manual recovery operations. Stopping the daemon does not stop billing. Archive cleanup can be enabled explicitly.

## Guides

- [Setup, credentials and configuration](docs/getting-started.md)
- [Project registration, placement, archive and recovery](docs/dispatch.md)
- [Run as a Home Manager service](docs/nix.md)
- [Architecture](docs/architecture.md) and [adding a provider type](docs/adding-a-provider.md)
- [Protocol limits](docs/roadmap.md), [validation record](docs/validation.md) and [security](docs/security.md)
- [Contributing](CONTRIBUTING.md)

## License

[MIT](LICENSE). The pairing and gateway design draws on [t3boat](https://github.com/paukraft/t3boat) by Pau Kraft; see [third-party notices](THIRD_PARTY_NOTICES.md). t3fleet is independent of T3 Code, Boat, and Namespace.

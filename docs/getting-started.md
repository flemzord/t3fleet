# Set up a fleet

The host runs `t3fleet serve` and a normal T3 server as its controller. Each thread runs on a separate remote worker. The controller, Desktop and worker image must speak the same T3 protocol; pin the controller and worker to `expectedVersion`. See the [release archives](https://github.com/flemzord/t3fleet/releases), [Nix installation](nix.md) or [source build](../CONTRIBUTING.md#development-setup).

## Prepare the workers

A **golden template** contains software and startup hooks, but no machine identity or user credentials. It must include:

- A Linux T3 server pinned to the version of the T3 desktop client you will use.
- The agent CLI for each driver in `dispatch.agents`: `codex`, `claude` (Claude Code) or `pi`. The reference image includes all three. On first boot it enables Pi in the worker's T3 settings; T3 enables Codex and Claude by default.
- A `t3fleet-runtime-info` executable available in the remote user's login shell.

The daemon executes this fixed command after provider startup:

```sh
/bin/bash -lc 'exec t3fleet-runtime-info'
```

The hook must start the already installed T3 server, wait for its descriptor, and emit exactly one JSON object on standard output:

```json
{
  "token": "PER_MACHINE_T3_SERVICE_TOKEN",
  "environmentId": "PER_MACHINE_T3_ENVIRONMENT_ID",
  "serverVersion": "VERSION_MATCHING_EXPECTED_VERSION"
}
```

Diagnostic messages belong on standard error. The server listens on `127.0.0.1:3773` inside the machine. Its service token is a private credential used only by the daemon. The daemon checks readiness through the provider's private transport and compares `serverVersion` with `expectedVersion`.

This repository ships self-contained image inputs:

- [`nix/t3-server.nix`](../nix/t3-server.nix) pins the Linux T3 server archive.
- [`nix/runtime.nix`](../nix/runtime.nix) supplies the startup, readiness, and credential-free image checks.
- [`images/Dockerfile`](../images/Dockerfile) combines them with Claude Code, Codex and Pi for a clean Namespace reference image.

The Linux `vm-tools` flake output is the common preinstalled profile. Build it on Linux or inside a provider image builder; the macOS development shell builds the daemon, not a Linux guest image.

For Boat, install the same `vm-tools` profile in a fresh credential-free builder, put its binaries on the remote user's login `PATH`, and run `t3fleet-image-check` before capturing a versioned saved snapshot. Do not start T3 in that golden builder. See the [reference image guide](../images/README.md) for publication details.

The daemon calls `t3fleet-runtime-info` after starting a machine. This hook starts the preinstalled T3 server idempotently before reporting readiness. That is local initialization, not package installation. A standalone VM that will never become a template may optionally use `t3fleet-server-start` as a boot hook.

Before publishing a shared image or snapshot, verify it contains none of:

- T3 databases, environment IDs, session tokens, or pairing state.
- Claude/Codex logins or API keys.
- Provider credentials, private SSH keys, repository tokens, `.env` files, or private project checkouts.

The reference image check rejects known credential and T3 state paths. It cannot prove that an arbitrary file contains no secret; keep the build environment credential-free as well.

Boat requires a ready saved snapshot. Namespace accepts a registered image (`registered:org/name`), an OCI image, or a golden Devbox source with an explicit completed snapshot. A Namespace image can bootstrap the pinned runtime on first use when Nix is already installed. The worker needs Git access to the repository and credentials for the chosen agent and model at run time. Fleet does not copy your desktop checkout or credentials.

## Configure the controller and placement

Start with [examples/boat.json](../examples/boat.json) or the [multi-provider example](../examples/config.json). The JSON file describes the fleet, not a list of projects or machines. Settings take effect when the service restarts.

| Setting | Meaning |
| --- | --- |
| `listen` | Loopback administration address; defaults to `127.0.0.1:47730`. |
| `stateDir` | Absolute path to private persistent state. The default is under `$XDG_STATE_HOME/t3fleet` or `~/.local/state/t3fleet`. |
| `expectedVersion` | Exact controller and worker T3 version, without the leading `v`. |
| `providers.<name>` | One configured account, template and machine shape. Names are operator-defined, e.g. `primary` and `burst` may both have `type: "boat"`. |
| `dispatch.listen` | Stable T3 endpoint, on a separate `127.0.0.1` port. Pair with this service once. |
| `dispatch.t3Binary` | Absolute path to the local T3 executable. |
| `dispatch.providerOrder` | Required array of instance names. The first is primary. Subsequent entries are fallbacks in order. |
| `dispatch.maxWorkers` | Maximum concurrent reservations, including pending launches. Omitted or zero means 8. |
| `dispatch.agents` | Agents offered by this fleet. Each entry has a T3 `driver` (`codex`, `claudeAgent` or `pi`) and the `models` it offers. A driver appears once. The first model of the first entry is the default for launches without a model. Pi model IDs use Pi's `<provider>/<model>` form, or `default`. Every worker image must include every declared agent. At startup, the daemon enables each declared agent in its local T3 controller settings so Desktop lists it. |
| `dispatch.model` | Shorthand for a single Codex model, used when `dispatch.agents` is absent. Defaults to `gpt-5.4`. Do not set both. |
| `dispatch.deleteWorkerOnArchive` | Default `false`. Set to `true` to delete a thread’s worker after a confirmed archive. |

**Boat.** Set `template.snapshot`. Optional `size` is `small`, `default`, `large`, or `xlarge`; omit it to use Boat’s default. Each instance requires `organization`, the Boat wallet to bill. To inject an existing Boat environment, set `environment`; otherwise the daemon requests a clean machine. Choose one of `apiKeyEnv`, `apiKeyFile`, or `credentialFile` per instance. An explicit selector must work; it will not fall back to another account. Without a selector, the daemon tries `BOAT_API_KEY` and the Boat login. Private credential files must not be group- or world-readable. See the [Boat adapter](providers-boat.md).

**Namespace.** Set `tenantId` to the ID of the Namespace tenant that owns these workers. The daemon checks that the token belongs to this tenant each time it obtains a credential. Set `template.image`, or both `template.source` and `template.snapshot`. Set `size` to `s`, `m`, `l`, or `xl`, or set `virtualCPU` and `memoryMB` for an explicit shape. The defaults are 4 vCPU, 8192 MiB and `amd64`. You can set `architecture` and `site`. Use `tokenFile` to select a credential for this instance; omit it to use the Namespace SDK login. See the [Namespace adapter](providers-namespace.md).

Credentials are loaded when a provider is used. A missing credential for an unused fallback does not prevent pairing, project registration, or launches on a healthy primary.

Keep an instance name tied to the same provider account and API endpoint for as long as workers or pending reservations use it. Add a new name for a different account. Reordering the list changes the policy for new threads. A timeout is not a reason to allocate another worker: see [placement and recovery](dispatch.md#placement-and-retry).

## Start and pair

```sh
export T3FLEET_CONFIG=/absolute/path/to/config.json
t3fleet validate
t3fleet serve
```

In another terminal with the same configuration, run `t3fleet pair` and open the one-use link in T3 Desktop. Use **Add to another machine** on a GitHub project. Choose the fleet, the default Git branch and a first text message. Adding another project needs no configuration change. The worker receives its own clone on the first launch. For server-to-server use see [Linked Environments](dispatch.md#launch-from-another-t3-server).

Use `t3fleet status` to see settings from the selected file and the reachability of the daemon, `t3fleet list` and `t3fleet inspect NAME` to locate a worker, and `t3fleet admin --help` for recovery. Keep the state directory and dispatch origin stable across restarts. The service only listens on loopback; a remote caller needs a private route, such as an SSH tunnel that preserves the port. Keep the admin listener private.

## Replacing an early configuration

The old top-level `templates`, `dispatchPOC`, dedicated worker pool, and `idleTimeout` settings are not accepted. Start from the new example and copy each template under its provider instance. If you have existing on-demand workers, keep the original state directory, listener, and account names (`boat` or `namespace`) so their persisted reservations still resolve. For a legacy worker without an account pin, startup checks the remote ID and name against that account. If it cannot confirm an existing allocation, reconcile it with the previous daemon and its original credentials before upgrading. Back up the private state before first startup: the new fleet state format cannot be opened by the previous daemon. Inspect or remove any old manually provisioned resources explicitly; changing a configuration does not delete cloud machines.

Older workers have no saved thread listing. Use `t3fleet list` to identify the worker, then `t3fleet admin start ID` to restore its thread in Desktop. This reads the selected worker only. Opening the fleet does not wake the others.

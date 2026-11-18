# Getting started

This guide starts a local daemon, creates one remote machine from a prepared template, and opens its association in T3 Code. It does not install software on every boot.

## 1. Build the CLI

```sh
nix develop
make lint
make test
make build
```

Alternatively, install Go 1.27 and a C compiler, then run the same Make targets. The binary is `bin/t3fleet`.

## 2. Prepare a template

A **golden template** contains software and startup hooks, but no machine identity or user credentials. It must include:

- A Linux T3 server pinned to the version of the T3 desktop client you will use.
- Claude Code and/or Codex if those providers will be used.
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
- [`images/Dockerfile`](../images/Dockerfile) combines them with Claude Code and Codex for a clean Namespace reference image.

The Linux `vm-tools` flake output is the common preinstalled profile. Build it on Linux or inside a provider image builder; the macOS development shell builds the daemon, not a Linux guest image.

For Namespace, prefer a persistent golden source Devbox and its completed snapshot. Install `vm-tools` once in a clean source, run `t3fleet-image-check`, then stop the source and wait for its snapshot to complete. The native Go adapter forks that snapshot when creating a machine. Keep T3 stopped in the source so each clone initializes its own identity.

A registered image is an alternative. To publish the reference image, use only a minimal build context:

```sh
context=$(mktemp -d)
cp flake.nix flake.lock "$context/"
cp -R nix "$context/"
cp images/Dockerfile "$context/Dockerfile"
devbox image build "$context" --name t3fleet/base --user devbox --optimize=false
```

The Devbox CLI is an optional image-publication tool; the running fleet daemon uses native APIs. Publication creates a billable provider build and can take longer than preparing a persistent source. Review the account and image name before running it, and remove the temporary local context when finished. Omit a T3 startup hook on any source that will become a golden snapshot.

For Boat, install the same `vm-tools` profile in a fresh credential-free builder, put its binaries on the remote user's login `PATH`, and run `t3fleet-image-check` before capturing a versioned saved snapshot. Do not start T3 in that golden builder. See the [reference image guide](../images/README.md) for publication details.

A larger development-tools profile exists in [flemzord/nixos-config](https://github.com/flemzord/nixos-config), but it is optional. t3fleet's reference image does not require that personal repository.

The daemon calls `t3fleet-runtime-info` after starting a machine. This hook starts the preinstalled T3 server idempotently before reporting readiness. That is local initialization, not package installation. A standalone VM that will never become a template may optionally use `t3fleet-server-start` as a boot hook.

Before publishing a shared image or snapshot, verify it contains none of:

- T3 databases, environment IDs, session tokens, or pairing state.
- Claude/Codex logins or API keys.
- Provider credentials, private SSH keys, repository tokens, `.env` files, or private project checkouts.

The reference image check rejects known credential and T3 state paths. It cannot prove that an arbitrary file contains no secret; keep the build environment credential-free as well.

### Boat template

Use a completed, versioned saved-snapshot name:

```json
"templates": {
  "boat": { "snapshot": "t3fleet-linux-T3_VERSION" }
}
```

A snapshot must be `ready`, not `saving` or `failed`. The `create` command uses the provider template from your configuration by default. To select another prepared Boat base explicitly:

```sh
./bin/t3fleet create --name my-agent --provider boat --snapshot SAVED_SNAPSHOT_NAME --config /absolute/path/to/config.json
```

### Namespace template

Prefer a golden source Devbox and one of its completed snapshots:

```json
"templates": {
  "namespace": {
    "source": "t3fleet-golden",
    "snapshot": "COMPLETED_SNAPSHOT_ID"
  }
}
```

Alternatively, use a registered image:

```json
"templates": {
  "namespace": { "image": "t3fleet/devbox" }
}
```

Omitting `snapshot` selects the source's latest completed snapshot. Namespace's inspected Devbox API does not expose an explicit manual-capture operation; verify that the desired capture is complete before using it. See [Namespace snapshot semantics](providers-namespace.md#images-and-snapshots).

## 3. Configure credentials and billing

Copy [examples/config.json](../examples/config.json), then replace its absolute `stateDir` and template placeholders. Pass `--config PATH` in each command. The explicit path works consistently on Linux and macOS; the implicit default follows the OS user-configuration directory.

Configuration is strict JSON: misspelled or unknown fields fail startup. For this release, both `listen` and machine endpoints must use loopback. Remove provider entries you do not use.

### Boat

The daemon loads Boat credentials in this order:

1. The configured `apiKeyEnv`, or `BOAT_API_KEY` when omitted.
2. `apiKeyFile`, if provided.
3. `credentialFile`, or Boat's existing credential file under the OS configuration directory.

For a private token file, use an absolute path and permissions `0600`; symbolic links and group/world-readable files are rejected. Do not put the token directly in `config.json`, an image, or this repository.

Set `providers.boat.organization` explicitly. When empty, the API follows the credential file's organization or Boat's active wallet. t3fleet does not switch that wallet. Make the billable organization a deliberate operator choice.

By default, `providers.boat.environment` is empty and creation sends `noEnv: true`. Boat account environments therefore do not inject repositories, model credentials, or secrets. To attach a specific Boat environment intentionally, set `environment` to its name; this sends `noEnv: false` and accepts that environment's contents. Keep this option empty for golden builders and ordinary clean machines.

An existing Boat CLI sign-in can supply credentials, but the daemon does not execute the CLI for lifecycle operations.

### Namespace

The official Go SDK loads an existing `devbox login` or `nsc login` session. It exchanges that session for a tenant token; the session file is not itself a raw bearer token.

For unattended use, configure an appropriate scoped credential through the SDK's supported `NSC_TOKEN_FILE` or workload-identity flow. Keep it outside the repository. See [Namespace credentials](providers-namespace.md#credentials).

### T3 version

Set `expectedVersion` to the exact server/client version, without a leading `v`. The reference default currently matches `0.1.11-nightly.20261117.4838`. A mismatch leaves the machine managed but not ready. Rebuild the golden template and update the configuration together after a compatibility test.

## 4. Start the daemon

```sh
./bin/t3fleet serve --config /absolute/path/to/config.json
```

The daemon binds its management API to `127.0.0.1:47730` by default. It creates a random admin token in its private state directory. Only one daemon can own that directory.

Keep this terminal or process running. Closing the daemon drops its local T3 connections but does not stop cloud machines. Each provider's billing and expiration policy still applies.

## 5. Create and pair a machine

In another terminal:

```sh
./bin/t3fleet create --name my-agent --provider boat --config /absolute/path/to/config.json
./bin/t3fleet list --config /absolute/path/to/config.json
```

Use `--provider namespace` for a Namespace machine. The create response includes the managed machine's `id`, `remoteId`, `state`, and stable local `url`. Use the managed `id` for CLI commands.

`create` prepares a one-use pairing link and opens it in T3 Code on the computer running the CLI. Confirm the backend in T3 to finish saving it. There is no separate `pair` step or manual link to copy in the normal flow. The machine JSON remains on standard output; progress and handoff instructions go to standard error. An interactive terminal shows an activity spinner and elapsed time during creation and readiness checks. Redirected output uses plain messages without terminal escape sequences. The indicator reports activity, not a completion percentage.

T3 deliberately [confirms an external pairing link](https://github.com/pingdotgg/t3code/blob/main/apps/web/src/components/desktop/DesktopWebLinkCoordinator.tsx) before saving the connection. t3fleet cannot currently remove this final click through a supported desktop API. A successful OS launch only means the link was handed off, not that the environment was registered.

For a headless host or a script, skip the desktop handoff:

```sh
./bin/t3fleet create --name my-agent --provider boat --no-register --config /absolute/path/to/config.json
./bin/t3fleet pair MACHINE_ID --config /absolute/path/to/config.json
```

The explicit `pair` command prints a `t3code://pair?...` link for manual use. Pairing codes expire after 15 minutes and can be redeemed once. Generating another link invalidates the previous unused link.

If creation succeeds but pairing or desktop launch fails, the CLI exits with an error and preserves the created machine ID in its JSON output and recovery message. Retry with `pair MACHINE_ID` and the same `--config`; do not rerun `create`. On Linux the automatic handoff requires `xdg-open` and a registered T3 Code URL handler. macOS uses `open`; Windows uses the OS URL handler.

The daemon persists the resulting T3 client authorization, so a normal daemon restart does not require pairing again. It must be able to reclaim the saved local port.

## 6. Connect models and work

This first release does not forward Claude/Codex login flows. Use the provider's normal remote terminal access to sign in **on the created machine**:

```sh
claude auth login
# or
codex login
```

Follow the vendor's remote/headless login instructions. Your subscription or API billing follows the authentication method you choose. Boat or Namespace bills the machine separately.

Add or clone your project using T3's supported project workflow or the remote terminal. t3fleet does not automatically upload your local working tree or `.env` files. Return changes through Git as usual.

## 7. Stop and remove resources

```sh
./bin/t3fleet stop MACHINE_ID --config /absolute/path/to/config.json
./bin/t3fleet start MACHINE_ID --config /absolute/path/to/config.json
./bin/t3fleet delete MACHINE_ID --config /absolute/path/to/config.json
```

`delete` is an explicit permanent deletion. Saved snapshots are separate provider resources and may remain after the VM is deleted. Remove unneeded snapshots through the provider when appropriate.

Authenticated requests for live T3 data can wake a stopped machine. Public discovery reads use the persisted descriptor and never wake the machine. There is no offline chat cache or agent-aware automatic sleep yet.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| CLI cannot reach daemon | Start `serve` with the same `--config` and state directory. |
| State directory already in use | Another daemon owns it. Do not delete its lock file to bypass ownership. |
| Create failed but returns a managed machine ID | Inspect it; `start ID` reconciles the durable creation operation. Do not invent a new name merely to retry a lost response. |
| Machine remains not ready | Verify the prepared runtime hook and exact T3 version. Cloud startup alone does not prove server readiness. |
| T3 cannot fetch a localhost endpoint | Keep the daemon running and ensure its saved port is available. |
| Pairing token is invalid | Generate a fresh link; an old code may have expired or been used. |
| Namespace `snapshot` is unsupported | Use an existing completed provider snapshot or image; explicit capture is not a capability of this adapter. |
| Machine still incurs charges after daemon exit | Stop or delete the remote resource explicitly; daemon shutdown is not VM shutdown. |


### Duplicate names and unprepared snapshots

A name stays reserved after a failed or interrupted creation. `create` reports a
conflict and, when supported by the running daemon, identifies the existing
record. Use `list` and `inspect ID` before retrying. Choose a different `--name`
for an independent VM. Repeating `create` does not replace an existing machine
or change its saved template. In particular, `start` on an interrupted creation
reuses its original template and operation identity.

A saved provider snapshot is not necessarily a t3fleet template. It must contain
T3 and `t3fleet-runtime-info` before use. If the fixed runtime command exits 127,
the daemon reports `runtime_unavailable` instead of a generic gateway error.
The allocated machine remains managed and billable. Prepare its runtime before
retrying `start`, or delete it before creating a replacement from a prepared
snapshot. Repeating `start` cannot install missing software.

After rebuilding the daemon, restart `t3fleet serve` with the same configuration
to use updated API errors. Replacing the CLI binary alone does not update an
already running daemon. Machine records and client authorization persist across
a normal daemon restart; cloud VMs are not stopped by that restart.

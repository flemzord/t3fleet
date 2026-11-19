# Contributing

Thanks for helping build t3fleet. The project is an early-alpha CLI and daemon for running T3 Code on provider-managed machines.

## Development setup

```sh
nix develop
make fmt
make lint
make test
make build
```

The Makefile uses Go from your `PATH` when available, or runs it through `nix develop`. Go 1.27 and a working C compiler can be used without Nix. `make test` runs the race detector. `make lint` validates `.golangci.yml` and runs the pinned golangci-lint, including gofmt, goimports and `go vet`; `make fmt` applies the formatters. CI runs lint and tests on Ubuntu and cross-compiles the release binaries for Linux and macOS.

Tests run without cloud credentials. Keep billable cloud canaries separate from the default suite, scoped to a dedicated test resource, and include cleanup and a verified final state.

## Releases

CI builds a GoReleaser snapshot without publishing it. To build the same archives locally, install the GoReleaser version pinned in [the release workflow](.github/workflows/release.yml), then run `goreleaser release --snapshot --clean` in the Go development environment. The archives and `checksums.txt` appear in `dist/`.
The release hook collects the resolved Go modules' and toolchain's license texts
into `THIRD_PARTY_LICENSES.txt` for each archive.

To publish a release, check out the commit you want to release and confirm its CI passed. Then create and push an annotated `v`-prefixed SemVer tag. For example, for the first alpha release:

```sh
git tag -a v0.1.0-alpha.1 -m 'v0.1.0-alpha.1'
git push origin v0.1.0-alpha.1
```

The Release workflow runs the Go checks, then publishes the archives, checksums, and changelog to GitHub Releases. Tags with a SemVer prerelease suffix produce GitHub prereleases. Check the workflow result and confirm that the release has all four archives and `checksums.txt`.

## Optional live validation

A genuine local T3 compatibility test uses a disposable home and no cloud account. Set an absolute path to an installed T3 executable:

```sh
T3FLEET_T3_BINARY="$HOME/.t3/bin/t3" \
  go test -race -run '^TestLiveT3Compatibility$' -v ./internal/proxy
```

This validates the server protocol, not a T3 desktop UI interaction.

The cloud lifecycle test creates **paid resources** using the accounts selected by a private config. Review its provider organizations, templates, and billing first. Use instance names from that configuration and enable only the accounts you intend to test:

```sh
T3FLEET_LIVE_CONFIG=/absolute/path/to/private/live-config.json \
T3FLEET_LIVE_PROVIDERS=primary,build \
  go test -race -run '^TestCloudLifecycle$' -v -timeout 30m ./internal/fleet
```

The test exercises creation, T3 readiness, pairing, HTTP, WebSocket, stop, discovery without wake, and resume. It attempts resource deletion during cleanup. Verify the final state with the provider even after failure or interruption; successful local process exit alone does not prove billing has stopped. Private recovery state remains under the configured `stateDir` in a `canary-<instance-name>` subdirectory for each selected instance. Preserve it if cleanup fails, reconcile any remaining remote machine, and do not delete it merely to allow another test run. Golden sources, published images, and saved snapshots are separate resources and require their own cleanup decision.

These tests are opt-in and must not run in pull-request CI. Record the exact T3 version and selected provider when reporting results. A skipped test is not live-validation evidence.

## Changes

- Keep cloud-specific behavior inside its provider adapter.
- Use the public `pkg/provider` contract for extensions and keep application internals under `internal`.
- Add regression tests for meaningful failure cases, not only the successful path.
- Update `docs` with behavior changes and keep README examples executable.
- Preserve explicit account/organization selection and avoid automatic retries of non-idempotent writes.
- Never commit credentials, private endpoint URLs, model logins, state files, or private project content.

Use Conventional Commit titles, for example `feat(namespace): support a new template option`. Describe the resulting behavior, relevant tests, and any remaining live-validation limits in a pull request. Do not include attribution to an AI model or agent in commits, issues, or pull requests.

For a new provider, start with [Adding a provider](docs/adding-a-provider.md). For protocol changes, read [the gateway contract](docs/protocol.md) and test the exact T3 client/server version you claim to support.

## Review expectations

A lifecycle change should answer:

- What happens after a timeout or lost response?
- Can a retry create a second resource or replay a write?
- Can an error, redirect, or diagnostic reveal credentials?
- Does cancellation close owned requests, streams, and tunnels?
- Does a failed delete preserve enough state to recover safely?

A successful mock test is useful evidence, but is not a claim that a provider machine or T3 desktop flow was verified live.

## Security reports

Follow [the security reporting guidance](docs/security.md#reporting-a-vulnerability). Do not publish credentials or a live exploit in an ordinary issue.

## License

Contributions are made under the repository's [MIT license](LICENSE). Preserve required third-party notices when adapting existing source.

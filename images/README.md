# Reference image

This optional image contains the pinned T3 server, Claude Code, Codex, Pi and the
`t3fleet-runtime-info` handshake. It uses only the public `t3fleet` flake and
Nixpkgs; it does not require a personal Nix configuration.

Prepare a minimal context so no Git history, `.env` file or local credentials
are uploaded to the image builder:

```sh
context=$(mktemp -d)
cp flake.nix flake.lock "$context/"
cp -R nix "$context/"
cp images/Dockerfile "$context/Dockerfile"
devbox image build "$context" --name t3fleet/base --user devbox \
  --optimize=false
```

Do not inject model credentials or start T3 during image construction. The
image check rejects existing T3 identity/database directories and provider
credentials. The controller calls `t3fleet-runtime-info` after boot; that command starts
the preinstalled server before the controller marks the machine ready. Do not
configure a startup hook on a golden source Devbox before snapshot/fork capture. No package is
downloaded at boot. T3 listens only on `127.0.0.1:3773`; use the provider's
private transport, not a public port.

The handshake uses `$HOME/.t3fleet/t3` for T3 data and stores one per-machine
bearer token in `$HOME/.t3fleet/service-token` with mode `0600`. Its JSON output
contains that token and must never be written to normal logs. Clone only a
credential-free golden image; snapshots of a used machine contain its private
identity and provider sessions and must stay bound to that machine.

For Boat, install the same `vm-tools` profile on a credential-free image
builder and run `t3fleet-image-check` before saving a named snapshot. The
controller invokes the runtime handshake after every create/resume. It does
not need a package installer in the boot path.

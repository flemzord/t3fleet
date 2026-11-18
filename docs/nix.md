# Install and run t3fleet with Nix

The flake builds the t3fleet CLI and daemon on Linux and macOS, on arm64 and x86-64. It pins its own build dependencies, so your system does not need to use the same nixpkgs revision. The T3 guest tools remain a separate Linux output.

## Install the CLI

With flakes enabled, install it in your Nix profile:

```sh
nix profile install github:flemzord/t3fleet#t3fleet
t3fleet version
```

On newer Nix versions, `nix profile add` is equivalent. To try it without changing your profile, use `nix run github:flemzord/t3fleet -- version`. In a local checkout, use `nix run . -- version`. The packaged CLI reports the release base and the source revision when Nix has revision metadata.

To install declaratively, add `inputs.t3fleet.url = "github:flemzord/t3fleet";` to your flake and use `inputs.t3fleet.packages.${pkgs.stdenv.hostPlatform.system}.t3fleet` in any package list. You can also add `inputs.t3fleet.overlays.default` to `nixpkgs.overlays` and install `pkgs.t3fleet`. A package-only install uses the [manual configuration and startup](getting-started.md#3-configure-credentials-and-billing).

## Enable the background service

Import this module in your Home Manager configuration. It works with standalone Home Manager and with Home Manager integrated into NixOS or nix-darwin. The module installs the CLI as well, so you can skip the profile install above.

```nix
{ config, inputs, ... }:
{
  imports = [ inputs.t3fleet.homeModules.default ];

  services.t3fleet = {
    enable = true;
    settings = {
      providers.boat = {
        organization = "YOUR_BOAT_ORGANIZATION";
        apiKeyFile = "${config.home.homeDirectory}/.secrets/boat-api-key";
      };
      templates.boat.snapshot = "YOUR_READY_BOAT_SNAPSHOT";
    };
  };
}
```

This example expects `inputs` in Home Manager's `extraSpecialArgs`. You can instead import the Fleet module directly from your flake's `modules` list.

Use the same keys as in [config.json](../examples/config.json). Enter credential **paths as strings**. Nix stores the generated JSON and service definition in the world-readable Nix store. Keep the Boat key in a regular file with mode `0600`. For a Namespace token file, set `services.t3fleet.namespaceTokenFile` to its absolute runtime path; the service sets `NSC_TOKEN_FILE`. For other Namespace authentication methods, see [Namespace credentials](providers-namespace.md#credentials). Use `services.t3fleet.environment` for non-secret variables such as `PATH`; its values also go into the Nix store.

Home Manager links the JSON into `$XDG_CONFIG_HOME/t3fleet/config.json` on Linux (`~/.config` by default) or `~/Library/Application Support/t3fleet/config.json` on macOS. On Linux, if you customize `xdg.configHome`, enable Home Manager's XDG settings and load the resulting session environment so the CLI sees the same path. The daemon gets its configuration path directly from the service definition. The default state directory is `$XDG_STATE_HOME/t3fleet` on Linux (`~/.local/state/t3fleet` by default), and `~/.local/state/t3fleet` on macOS.

If you already run Fleet manually, move the existing config file out of the managed location, transfer your settings into Nix, and set `settings.stateDir` to the old state directory. Stop the manual daemon before switching so the service can take ownership of that state. If you used a custom LaunchAgent or systemd unit, disable and remove it as part of the migration so it cannot restart and compete for the same address.

Apply the configuration with your usual Home Manager, NixOS, or nix-darwin switch command. Then run `t3fleet list` as the same user. An empty list shows that the CLI reached the daemon; it does not require creating a cloud machine.

| Platform | Inspect the service | Read logs |
| --- | --- | --- |
| Linux | `systemctl --user status t3fleet` | `journalctl --user -u t3fleet` |
| macOS | `launchctl print gui/$(id -u)/com.t3fleet.daemon` | `tail -f ~/Library/Logs/t3fleet.log` |

On Linux the systemd user service starts with the user manager. If it should run from boot and between logins, enable lingering for that account with `loginctl enable-linger "$USER"` where your host policy allows it. On macOS the LaunchAgent starts at login and runs while the user session is open.

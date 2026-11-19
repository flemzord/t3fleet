self: { config, lib, pkgs, ... }:

let
  cfg = config.services.t3fleet;
  host = pkgs.stdenv.hostPlatform;
  home = config.home.homeDirectory;
  configHome = if host.isDarwin then "${home}/Library/Application Support" else config.xdg.configHome;
  settingsFormat = pkgs.formats.json { };
  rendered = settingsFormat.generate "t3fleet-settings.json" cfg.settings;
  configFile = pkgs.runCommand "t3fleet-config.json" { } ''
    ${lib.getExe cfg.package} validate --config ${rendered}
    cp ${rendered} "$out"
  '';
  command = [ (lib.getExe cfg.package) "serve" "--config" (toString configFile) ];
  logFile = "${home}/Library/Logs/t3fleet.log";
  environment = cfg.environment // lib.optionalAttrs (cfg.namespaceTokenFile != null) {
    NSC_TOKEN_FILE = cfg.namespaceTokenFile;
  };
  # Environment= uses systemd quoting and expands % specifiers.
  systemdEnv = value: builtins.toJSON (lib.replaceStrings [ "%" ] [ "%%" ] value);
in
{
  options.services.t3fleet = {
    enable = lib.mkEnableOption "the t3fleet user service";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${host.system}.t3fleet;
      defaultText = lib.literalExpression "t3fleet.packages.\${system}.t3fleet";
      description = "The t3fleet package to install and run.";
    };

    settings = lib.mkOption {
      inherit (settingsFormat) type;
      default = { };
      apply = settings:
        if host.isLinux && builtins.isAttrs (settings.dispatch or null)
          && !(settings.dispatch ? t3Binary)
        then settings // {
          dispatch = settings.dispatch // {
            t3Binary = lib.getExe self.packages.${host.system}.t3-server;
          };
        } else settings;
      description = ''
        On-demand fleet configuration as in examples/config.json. On Linux,
        dispatch.t3Binary defaults to the T3 server pinned by this flake.
        On macOS, set it to the absolute path of a matching T3 executable.
        This JSON is written to the Nix store. Use absolute string paths
        to credential files; never put secrets or Nix path literals here.
      '';
    };

    environment = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      example = { PATH = "/run/current-system/sw/bin:/usr/bin:/bin"; };
      description = ''
        Extra non-secret environment variables for the daemon. Values are
        stored in the Nix store. Use credential files for secrets.
      '';
    };

    namespaceTokenFile = lib.mkOption {
      type = lib.types.nullOr (lib.types.strMatching "^/[^\n\r]+$");
      default = null;
      example = "/run/user/1000/secrets/namespace-token";
      description = ''
        Absolute runtime path for the Namespace SDK's NSC_TOKEN_FILE.
        Only the path is stored in the service definition. This option
        takes precedence over environment.NSC_TOKEN_FILE.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [ {
      assertion = builtins.isAttrs (cfg.settings.providers or null) && cfg.settings.providers != { };
      message = "services.t3fleet.settings.providers must contain at least one named provider instance.";
    } {
      assertion = lib.all (name: builtins.match "[A-Za-z_][A-Za-z0-9_]*" name != null)
        (builtins.attrNames cfg.environment);
      message = "services.t3fleet.environment must contain valid environment variable names.";
    } ];
    home.packages = [ cfg.package ];
    home.file."${configHome}/t3fleet/config.json".source = configFile;
    services.t3fleet.settings.stateDir = lib.mkDefault (
      if host.isDarwin then "${home}/.local/state/t3fleet" else "${config.xdg.stateHome}/t3fleet"
    );

    systemd.user.services.t3fleet = lib.mkIf host.isLinux {
      Unit.Description = "T3 Fleet daemon";
      Install.WantedBy = [ "default.target" ];
      Service = {
        ExecStart = lib.concatStringsSep " " command;
        Restart = "on-failure";
        RestartSec = "5s";
        UMask = "0077";
        Environment = lib.mapAttrsToList (name: value: systemdEnv "${name}=${value}")
          ({ XDG_CONFIG_HOME = configHome; } // environment);
      };
    };

    launchd.agents.t3fleet = lib.mkIf host.isDarwin {
      enable = true;
      config = {
        Label = "com.t3fleet.daemon";
        ProgramArguments = command;
        RunAtLoad = true;
        KeepAlive = true;
        ThrottleInterval = 10;
        Umask = 63; # 0077
        StandardOutPath = logFile;
        StandardErrorPath = logFile;
        EnvironmentVariables = environment;
      };
    };
    home.activation.t3fleetLogDirectory = lib.mkIf host.isDarwin (
      lib.hm.dag.entryBetween [ "setupLaunchAgents" ] [ "writeBoundary" ] ''
        run mkdir -p -- ${lib.escapeShellArg "${home}/Library/Logs"}
      ''
    );
  };
}

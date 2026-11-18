self: { config, lib, pkgs, ... }:

let
  cfg = config.services.t3fleet;
  host = pkgs.stdenv.hostPlatform;
  home = config.home.homeDirectory;
  configHome = if host.isDarwin then "${home}/Library/Application Support" else config.xdg.configHome;
  settingsFormat = pkgs.formats.json { };
  configFile = settingsFormat.generate "t3fleet-config.json" cfg.settings;
  command = [ (lib.getExe cfg.package) "serve" "--config" (toString configFile) ];
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
      description = ''
        Fleet configuration as in examples/config.json. This JSON is written
        to the Nix store. Use absolute string paths to credential files;
        never put secrets or Nix path literals here.
      '';
    };

    namespaceTokenFile = lib.mkOption {
      type = lib.types.nullOr (lib.types.strMatching "^/[^\n\r]+$");
      default = null;
      example = "/run/user/1000/secrets/namespace-token";
      description = ''
        Absolute runtime path for the Namespace SDK's NSC_TOKEN_FILE.
        Only the path is stored in the service definition.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [ {
      assertion = (cfg.settings.providers.boat or null) != null
        || (cfg.settings.providers.namespace or null) != null;
      message = "services.t3fleet.settings.providers must configure Boat or Namespace.";
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
        Environment = [ (systemdEnv "XDG_CONFIG_HOME=${configHome}") ]
          ++ lib.optional (cfg.namespaceTokenFile != null) (systemdEnv "NSC_TOKEN_FILE=${cfg.namespaceTokenFile}");
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
        StandardErrorPath = "${home}/Library/Logs/t3fleet.log";
      } // lib.optionalAttrs (cfg.namespaceTokenFile != null) {
        EnvironmentVariables.NSC_TOKEN_FILE = cfg.namespaceTokenFile;
      };
    };
    home.activation.t3fleetLogDirectory = lib.mkIf host.isDarwin (
      lib.hm.dag.entryBetween [ "setupLaunchAgents" ] [ "writeBoundary" ] ''
        run mkdir -p -- ${lib.escapeShellArg "${home}/Library/Logs"}
      ''
    );
  };
}

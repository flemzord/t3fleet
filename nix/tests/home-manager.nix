# Evaluate with `nix eval --impure --json --expr 'import ./nix/tests/home-manager.nix {}'`.
{ system ? builtins.currentSystem
, flakeRef ? if builtins.pathExists ../../.git then "git+file://${toString ../..}" else "path:${toString ../..}"
, homeDirectory ? "/tmp/t3fleet-test"
, listenPort ? "47730"
, dispatchPort ? "47733"
, darwinT3Binary ? "/opt/test/t3"
, namespaceTokenFile ? "/run/user/1000/t3fleet token%file"
}:

let
  fleet = builtins.getFlake flakeRef;
  extraValue = "/tmp/fleet tools%/bin";
  # Keep the test dependency separate from the public flake inputs.
  hm = builtins.getFlake "github:nix-community/home-manager/5643e73425b85ac359ba2aaf83deae8ce1cd7968";
  pkgs = import fleet.inputs.nixpkgs { inherit system; };
  darwin = pkgs.stdenv.hostPlatform.isDarwin;
  hmConfig = hm.lib.homeManagerConfiguration {
    inherit pkgs;
    modules = [
      fleet.homeModules.default
      {
        home = {
          username = "fleet-test";
          inherit homeDirectory;
          stateVersion = "26.05";
        };
        xdg = {
          enable = true;
          configHome = "${homeDirectory}/custom config";
          stateHome = "${homeDirectory}/custom state";
        };
        services.t3fleet = {
          enable = true;
          inherit namespaceTokenFile;
          environment.T3FLEET_TEST_ENV = extraValue;
          settings = {
            listen = "127.0.0.1:${listenPort}";
            providers.primary = {
              type = "boat";
              template.snapshot = "test-template";
              apiKeyEnv = "T3FLEET_TEST_BOAT_KEY";
              organization = "t3fleet-test";
              baseURL = "http://127.0.0.1:1";
            };
            # A missing credential for an unused backup must not block the controller.
            providers.reserve = {
              type = "namespace";
              tenantId = "test-tenant";
              tokenFile = "${homeDirectory}/missing-namespace-token";
              endpoint = "http://127.0.0.1:1";
              template.image = "registered:example/worker";
            };
            dispatch = {
              listen = "127.0.0.1:${dispatchPort}";
              providerOrder = [ "primary" "reserve" ];
              maxWorkers = 2;
            } // pkgs.lib.optionalAttrs darwin {
              t3Binary = darwinT3Binary;
            };
          };
        };
      }
    ];
  };
  cfg = hmConfig.config;
  expectedConfig = if darwin then "${homeDirectory}/Library/Application Support" else cfg.xdg.configHome;
  expectedState = if darwin then "${homeDirectory}/.local/state" else cfg.xdg.stateHome;
  configPath = "${expectedConfig}/t3fleet/config.json";
  configFile = cfg.home.file.${configPath}.source;
  package = fleet.packages.${system}.t3fleet;
  logFile = "${homeDirectory}/Library/Logs/t3fleet.log";
  systemdEnv = value: builtins.toJSON (builtins.replaceStrings [ "%" ] [ "%%" ] value);
  command = [ "${package}/bin/t3fleet" "serve" "--config" (toString configFile) ];
  job = cfg.launchd.agents.t3fleet.config;
  unit = cfg.systemd.user.services.t3fleet;
  serviceFile = if darwin then "${hmConfig.activationPackage}/LaunchAgents/com.t3fleet.daemon.plist"
    else cfg.xdg.configFile."systemd/user/t3fleet.service".source;
in
assert builtins.elem package cfg.home.packages;
assert cfg.services.t3fleet.settings.stateDir == "${expectedState}/t3fleet";
assert cfg.services.t3fleet.settings.dispatch.t3Binary ==
  (if darwin then darwinT3Binary else "${fleet.packages.${system}.t3-server}/bin/t3");
assert if darwin then job.ProgramArguments == command && job.RunAtLoad && job.KeepAlive
  && job.Umask == 63
  && job.StandardOutPath == logFile && job.StandardErrorPath == logFile
  && job.EnvironmentVariables.T3FLEET_TEST_ENV == extraValue
  && (job.EnvironmentVariables.NSC_TOKEN_FILE or null) == namespaceTokenFile
else unit.Service.ExecStart == [ (builtins.concatStringsSep " " command) ]
  && unit.Install.WantedBy == [ "default.target" ]
  && unit.Service.Restart == "on-failure"
  && unit.Service.UMask == "0077"
  && builtins.elem (systemdEnv "T3FLEET_TEST_ENV=${extraValue}") unit.Service.Environment
  && (namespaceTokenFile == null || builtins.elem
    (systemdEnv "NSC_TOKEN_FILE=${namespaceTokenFile}")
    unit.Service.Environment);
{
  inherit configPath configFile;
  service = pkgs.runCommand "t3fleet-home-manager-service" { } ''
    cp ${serviceFile} "$out"
  '';
}

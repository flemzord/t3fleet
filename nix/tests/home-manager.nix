# Evaluate with `nix eval --impure --json --expr 'import ./nix/tests/home-manager.nix {}'`.
{ system ? builtins.currentSystem
, homeDirectory ? "/tmp/t3fleet-test"
, listenPort ? "47730"
, namespaceTokenFile ? "/run/user/1000/t3fleet token%file"
}:

let
  fleet = builtins.getFlake ("path:" + toString ../..);
  # Keep the test dependency separate from the public flake inputs.
  hm = builtins.getFlake "github:nix-community/home-manager/5643e73425b85ac359ba2aaf83deae8ce1cd7968";
  pkgs = import fleet.inputs.nixpkgs { inherit system; };
  darwin = pkgs.stdenv.hostPlatform.isDarwin;
  hmConfig = hm.lib.homeManagerConfiguration {
    inherit pkgs;
    modules = [
      fleet.homeManagerModules.default
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
          settings = {
            listen = "127.0.0.1:${listenPort}";
            providers.boat = {
              apiKeyEnv = "T3FLEET_TEST_BOAT_KEY";
              apiKeyFile = "${homeDirectory}/boat-key";
              organization = "t3fleet-test";
              baseURL = "http://127.0.0.1:1";
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
  command = [ "${package}/bin/t3fleet" "serve" "--config" (toString configFile) ];
  job = cfg.launchd.agents.t3fleet.config;
  unit = cfg.systemd.user.services.t3fleet;
  serviceFile = if darwin then "${hmConfig.activationPackage}/LaunchAgents/com.t3fleet.daemon.plist"
    else cfg.xdg.configFile."systemd/user/t3fleet.service".source;
in
assert builtins.elem package cfg.home.packages;
assert cfg.services.t3fleet.settings.stateDir == "${expectedState}/t3fleet";
assert if darwin then job.ProgramArguments == command && job.RunAtLoad && job.KeepAlive
  && (job.EnvironmentVariables.NSC_TOKEN_FILE or null) == namespaceTokenFile
else unit.Service.ExecStart == [ (builtins.concatStringsSep " " command) ]
  && unit.Install.WantedBy == [ "default.target" ]
  && unit.Service.Restart == "on-failure"
  && (namespaceTokenFile == null || builtins.elem
    (builtins.toJSON ("NSC_TOKEN_FILE=" + builtins.replaceStrings [ "%" ] [ "%%" ] namespaceTokenFile))
    unit.Service.Environment);
{
  inherit configPath configFile;
  service = pkgs.runCommand "t3fleet-home-manager-service" { } ''
    cp ${serviceFile} "$out"
  '';
}

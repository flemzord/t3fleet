{
  description = "t3fleet CLI, user service, and T3 guest tools";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/f5c082a40f7571c266e74e80ae2e68aadd8a9fc7";
  outputs = { self, nixpkgs, ... }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      eachSystem = nixpkgs.lib.genAttrs systems;
      version = "0.1.0-rc.4+${self.shortRev or self.dirtyShortRev or "source"}";
    in
    {
      overlays.default = final: _: {
        t3fleet = self.packages.${final.stdenv.hostPlatform.system}.t3fleet;
      };
      homeModules.default = import ./nix/home-manager.nix self;
      homeManagerModules = self.homeModules;

      devShells = eachSystem (system:
        let pkgs = import nixpkgs { inherit system; };
        in {
          default = pkgs.mkShell {
            packages = [ pkgs.go pkgs.gnumake pkgs.git ];
            # The locked nixpkgs Go bootstraps the exact supported toolchain.
            GOTOOLCHAIN = "go1.27.3";
          };
        });
      packages = eachSystem (system:
        let
          pkgs = import nixpkgs {
            inherit system;
            config.allowUnfreePredicate = pkg: builtins.elem (nixpkgs.lib.getName pkg) [ "claude-code" ];
          };
        in
        rec {
          t3fleet = pkgs.callPackage ./nix/t3fleet.nix { inherit version; };
          default = t3fleet;
        } // nixpkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux rec {
          t3-server = pkgs.callPackage ./nix/t3-server.nix { };
          runtime = pkgs.callPackage ./nix/runtime.nix { };
          vm-tools = pkgs.buildEnv {
            name = "t3fleet-vm-tools";
            paths = [ t3-server runtime pkgs.codex pkgs.claude-code pkgs.pi-coding-agent ];
          };
        });
      checks = eachSystem (system:
        let pkgs = import nixpkgs { inherit system; };
        in { runtime = pkgs.callPackage ./nix/runtime-tests.nix { }; });
    };
}

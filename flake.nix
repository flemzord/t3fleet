{
  description = "t3fleet Go development environment";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/f5c082a40f7571c266e74e80ae2e68aadd8a9fc7";
  outputs = { nixpkgs, ... }:
    let
      systems = [ "aarch64-darwin" "aarch64-linux" "x86_64-linux" ];
      eachSystem = nixpkgs.lib.genAttrs systems;
    in
    {
      devShells = eachSystem (system:
        let pkgs = import nixpkgs { inherit system; };
        in { default = pkgs.mkShell { packages = [ pkgs.go pkgs.gnumake pkgs.git ]; }; });
      packages = eachSystem (system:
        let
          pkgs = import nixpkgs {
            inherit system;
            config.allowUnfreePredicate = pkg: builtins.elem (nixpkgs.lib.getName pkg) [ "claude-code" ];
          };
        in
        nixpkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux rec {
          t3-server = pkgs.callPackage ./nix/t3-server.nix { };
          runtime = pkgs.callPackage ./nix/runtime.nix { };
          vm-tools = pkgs.buildEnv {
            name = "t3fleet-vm-tools";
            paths = [ t3-server runtime pkgs.codex pkgs.claude-code ];
          };
        });
      checks = eachSystem (system:
        let pkgs = import nixpkgs { inherit system; };
        in { runtime = pkgs.callPackage ./nix/runtime-tests.nix { }; });
    };
}

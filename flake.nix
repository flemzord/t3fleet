{
  description = "t3fleet Go development environment";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/f5c082a40f7571c266e74e80ae2e68aadd8a9fc7";
  outputs = { nixpkgs, ... }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      eachSystem = nixpkgs.lib.genAttrs systems;
    in
    {
      devShells = eachSystem (system:
        let pkgs = import nixpkgs { inherit system; };
        in { default = pkgs.mkShell { packages = [ pkgs.go pkgs.gnumake pkgs.git ]; }; });
    };
}

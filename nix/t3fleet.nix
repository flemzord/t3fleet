{
  lib,
  buildGo127Module,
  version ? "dev",
}:

buildGo127Module {
  pname = "t3fleet";
  inherit version;

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../cmd
      ../internal
      ../pkg
      ../go.mod
      ../go.sum
      ../runtime_bundle.go
      # The daemon embeds the guest flake for Namespace fallback workers.
      ../flake.nix
      ../flake.lock
      ./runtime.nix
      ./t3-server.nix
      ./runtime-tests.nix
    ];
  };
  goSum = ../go.sum;
  vendorHash = "sha256-xCubB6QPHsqytCx3OKUn83rnE8MjDuQf/+DxzKafe5U=";

  subPackages = [ "cmd/t3fleet" ];
  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];

  meta = {
    description = "Manage remote T3 Code machines";
    homepage = "https://github.com/flemzord/t3fleet";
    license = lib.licenses.mit;
    mainProgram = "t3fleet";
    platforms = lib.platforms.linux ++ lib.platforms.darwin;
  };
}

{
  lib,
  buildGo127Module,
  version,
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

  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    actual="$("$out/bin/t3fleet" version)"
    expected=${lib.escapeShellArg "t3fleet ${version}"}
    if [ "$actual" != "$expected" ]; then
      printf 'Expected version %s, got %s\n' "$expected" "$actual" >&2
      exit 1
    fi
    runHook postInstallCheck
  '';

  meta = {
    description = "Run T3 Code workers on demand";
    homepage = "https://github.com/flemzord/t3fleet";
    license = lib.licenses.mit;
    mainProgram = "t3fleet";
    platforms = lib.platforms.linux ++ lib.platforms.darwin;
  };
}

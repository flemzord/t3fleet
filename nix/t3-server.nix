{ autoPatchelfHook
, fetchurl
, git
, lib
, makeWrapper
, openssh
, stdenv
, stdenvNoCC
, vulkan-loader
}:

let
  version = "0.1.11-nightly.20261117.4838";
  assets = {
    x86_64-linux = {
      arch = "x64";
      sha256 = "66269980db58cbf7051155c5cb6995f0a6286441e74fca2e911283dd0e070f1f";
    };
    aarch64-linux = {
      arch = "arm64";
      sha256 = "314f6e58da9f7e799622997d0d8e63f51cc68e15eb78c623232be8fa13cd76bc";
    };
  };
  asset = assets.${stdenvNoCC.hostPlatform.system}
    or (throw "t3-server: only Linux x86_64 and aarch64 are supported");
in
stdenvNoCC.mkDerivation {
  pname = "t3-server";
  inherit version;
  src = fetchurl {
    url = "https://github.com/pingdotgg/t3code/releases/download/v${version}/t3-${version}-linux-${asset.arch}.tar.gz";
    inherit (asset) sha256;
  };
  nativeBuildInputs = [ autoPatchelfHook makeWrapper ];
  buildInputs = [ stdenv.cc.cc.lib vulkan-loader ];
  dontConfigure = true;
  dontBuild = true;
  # GNU strip rewrites .note.node.sea outside its segment and corrupts the
  # embedded Node SEA payload. Patchelf alone preserves it; keep this binary
  # intact and let the install check catch any future payload regression.
  dontStrip = true;

  installPhase = ''
    runHook preInstall
    mkdir -p "$out/libexec/t3" "$out/bin"
    cp -R . "$out/libexec/t3/"
    # These alternative prebuilds target musl, while our images use glibc.
    rm -rf "$out/libexec/t3/node_modules/@yuuang/ffi-rs-linux-${asset.arch}-musl" \
      "$out/libexec/t3/node_modules/@ff-labs/fff-bin-linux-${asset.arch}-musl" \
      "$out/libexec/t3/node_modules/@koromix/koffi-linux-${asset.arch}/musl_${asset.arch}"
    # Upstream resolves native helpers relative to its real executable.
    makeWrapper "$out/libexec/t3/t3" "$out/bin/t3" \
      --prefix PATH : ${lib.makeBinPath [ git openssh ]}
    runHook postInstall
  '';

  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    test "$("$out/bin/t3" --version)" = "t3 v${version}"
    "$out/bin/t3" serve --help >/dev/null
    runHook postInstallCheck
  '';

  meta = {
    description = "Pinned self-contained T3 Code server for t3fleet development images";
    homepage = "https://github.com/pingdotgg/t3code";
    license = lib.licenses.mit;
    mainProgram = "t3";
    platforms = builtins.attrNames assets;
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
  };
}

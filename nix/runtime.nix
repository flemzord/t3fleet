{ pkgs, t3Server ? pkgs.callPackage ./t3-server.nix { } }:

let
  infoProgram = pkgs.writeText "t3fleet-runtime-info.py" ''
    import json
    import os
    from pathlib import Path
    import stat
    import subprocess
    import sys
    import tempfile
    import urllib.error
    import urllib.request

    root = Path.home() / ".t3fleet"
    home = Path(os.environ.get("T3CODE_HOME", str(root / "t3")))
    port = int(os.environ.get("T3FLEET_T3_PORT", "3773"))
    origin = "http://127.0.0.1:" + str(port)
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    token_file = root / "service-token"

    def request(path, token=None):
        headers = {"Authorization": "Bearer " + token} if token else {}
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(urllib.request.Request(origin + path, headers=headers), timeout=10) as response:
            return json.load(response)

    try:
        descriptor = request("/.well-known/t3/environment")
        if descriptor["serverVersion"] != "${t3Server.version}":
            raise RuntimeError("T3 server version does not match the image")
        token = None
        if token_file.exists() or token_file.is_symlink():
            metadata = token_file.lstat()
            if not stat.S_ISREG(metadata.st_mode) or metadata.st_uid != os.getuid() or metadata.st_mode & 0o077:
                raise RuntimeError("Unsafe t3fleet token file")
            token = token_file.read_text().strip()
            try:
                request("/api/auth/session", token)
            except urllib.error.HTTPError as error:
                if error.code not in (401, 403):
                    raise
                token = None
        if not token:
            result = subprocess.run(
                ["${t3Server}/bin/t3", "auth", "session", "issue", "--base-dir", str(home),
                 "--ttl", "30d", "--label", "t3fleet", "--token-only"],
                check=True, capture_output=True, text=True, timeout=30,
            )
            token = result.stdout.strip()
            if not token or any(character.isspace() for character in token):
                raise RuntimeError("T3 did not issue one bearer token")
            descriptor_fd, temporary = tempfile.mkstemp(prefix=".service-token-", dir=root)
            with os.fdopen(descriptor_fd, "w") as stream:
                stream.write(token + "\n")
            os.replace(temporary, token_file)
        print(json.dumps({"token": token, "environmentId": descriptor["environmentId"],
                          "serverVersion": descriptor["serverVersion"]}))
    except (KeyError, OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print("Cannot initialize t3fleet runtime: " + type(error).__name__, file=sys.stderr)
        sys.exit(1)
  '';
  start = pkgs.writeShellApplication {
    name = "t3fleet-server-start";
    runtimeInputs = [ t3Server pkgs.coreutils pkgs.curl pkgs.util-linux ];
    text = ''
      # Called by the provider boot hook after restoring the golden image.
      # No packages are downloaded and no identity is created at image build.
      export T3CODE_HOME="''${T3CODE_HOME:-$HOME/.t3fleet/t3}"
      port="''${T3FLEET_T3_PORT:-3773}"
      host="''${T3FLEET_T3_HOST:-127.0.0.1}"
      if [[ ! "$port" =~ ^[0-9]+$ ]] || [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
        echo "T3FLEET_T3_PORT must be between 1 and 65535." >&2
        exit 1
      fi
      case "$host" in 127.0.0.1|0.0.0.0) ;; *) echo "Unsupported T3 bind address." >&2; exit 1 ;; esac
      case "$T3CODE_HOME" in /*) ;; *) echo "T3CODE_HOME must be absolute." >&2; exit 1 ;; esac
      install -d -m 0700 "$T3CODE_HOME"
      exec 9>"$T3CODE_HOME/start.lock"
      flock -x 9
      ready_url="http://127.0.0.1:$port/.well-known/t3/environment"
      settings="$T3CODE_HOME/userdata/settings.json"
      if [ ! -e "$settings" ] && [ ! -L "$settings" ]; then
        # T3 enables Codex and Claude by default but not Pi. Seed the first
        # boot only; later edits on the worker are kept.
        install -d -m 0700 "$T3CODE_HOME/userdata"
        (umask 077 && printf '%s\n' '{"providerInstances":{"codex":{"driver":"codex"},"claudeAgent":{"driver":"claudeAgent"},"pi":{"driver":"pi","enabled":true}}}' >"$settings")
      fi
      if ! curl --noproxy '*' -fsS --max-time 2 "$ready_url" >/dev/null 2>&1; then
        # Namespace image sessions do not require systemd. `setsid` also closes
        # the provider exec session; this process survives its disconnect.
        nohup setsid t3 serve --mode desktop --host "$host" --port "$port" \
          --base-dir "$T3CODE_HOME" >"$T3CODE_HOME/server.log" 2>&1 </dev/null 8>&- 9>&- &
        printf '%s\n' "$!" > "$T3CODE_HOME/server.pid"
      fi
      for _ in $(seq 1 120); do
        if curl --noproxy '*' -fsS --max-time 2 "$ready_url" >/dev/null 2>&1; then
          echo "T3 server ready on port $port"
          exit 0
        fi
        sleep 0.5
      done
      echo "T3 server did not become ready; inspect $T3CODE_HOME/server.log." >&2
      exit 1
    '';
  };
  clean = pkgs.writeShellApplication {
    name = "t3fleet-image-check";
    runtimeInputs = [ pkgs.coreutils ];
    text = ''
      for variable in AI_RELAY_API_KEY OPENAI_API_KEY ANTHROPIC_API_KEY CLAUDE_CODE_OAUTH_TOKEN CODEX_ACCESS_TOKEN; do
        if [ -n "''${!variable:-}" ]; then
          echo "Image builder contains a runtime provider credential in its environment." >&2
          exit 1
        fi
      done
      # Reject state rather than deleting it: a mistaken check against a live
      # machine must not destroy its sessions or credentials.
      for directory in "$HOME/.t3" "$HOME/.t3fleet" "$HOME/.t3boat"; do
        if [ -e "$directory" ] || [ -L "$directory" ]; then
          echo "Image contains runtime T3 state: $directory" >&2
          exit 1
        fi
      done
      for credential in "$HOME/.codex/auth.json" "$HOME/.claude/.credentials.json" \
        "$HOME/.pi/agent/auth.json" "$HOME/.config/ai-relay/client-key"; do
        if [ -e "$credential" ] || [ -L "$credential" ]; then
          echo "Image contains runtime provider credentials." >&2
          exit 1
        fi
      done
      test "$(t3 --version)" = "t3 v${t3Server.version}"
      for agent in codex claude pi; do
        executable=$(command -v "$agent")
        case "$(readlink -f "$executable")" in
          /nix/store/*) ;;
          *) echo "An agent is not managed by Nix: $agent" >&2; exit 1 ;;
        esac
      done
      echo "t3fleet image is credential-free and contains the pinned server."
    '';
  };
  info = pkgs.writeShellApplication {
    name = "t3fleet-runtime-info";
    runtimeInputs = [ start pkgs.python3 pkgs.coreutils pkgs.util-linux ];
    text = ''
      # This command is the controller's runtime handshake. Only its final JSON
      # goes to stdout; readiness diagnostics stay on stderr.
      install -d -m 0700 "$HOME/.t3fleet"
      exec 8>"$HOME/.t3fleet/runtime-info.lock"
      flock -x 8
      t3fleet-server-start >&2
      exec python3 ${infoProgram}
    '';
  };
in
pkgs.buildEnv {
  name = "t3fleet-runtime";
  paths = [ start clean info ];
  passthru = { inherit start clean info infoProgram; };
}

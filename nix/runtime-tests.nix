{ pkgs }:

let
  fakeT3Program = pkgs.writeText "fake-t3.py" ''
    import http.server
    import json
    import os
    from pathlib import Path
    import sys

    if len(sys.argv) > 1 and sys.argv[1] == "serve":
        port = int(sys.argv[sys.argv.index("--port") + 1])
        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_GET(self):
                if self.path == "/.well-known/t3/environment":
                    result = {"serverVersion": "fixture", "environmentId": "env-startup-fixture"}
                elif self.path == "/api/auth/session" and self.headers.get("Authorization") == "Bearer fixture-token":
                    result = {"authenticated": True}
                else:
                    self.send_response(401)
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(result).encode())
        Path(os.environ["HOME"], "mock-server.pid").write_text(str(os.getpid()))
        http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
    elif len(sys.argv) > 1 and sys.argv[1] == "--version":
        print("t3 vfixture")
    else:
        with open(os.environ["TEST_MINT_LOG"], "a") as stream:
            stream.write("mint\n")
        print("fixture-token")
  '';
  fakeT3 = (pkgs.writeShellScriptBin "t3" ''
    exec ${pkgs.python3}/bin/python3 ${fakeT3Program} "$@"
  '') // { version = "fixture"; };
  # Exercise real kernel flock/session semantics on both Darwin and Linux.
  # These adapters implement only the utility flags the production script uses.
  kernelUtilities = pkgs.symlinkJoin {
    name = "t3fleet-test-kernel-utilities";
    paths = [
      (pkgs.writeScriptBin "flock" ''
        #!${pkgs.python3}/bin/python3
        import fcntl
        import sys
        assert sys.argv[1] == "-x"
        fcntl.flock(int(sys.argv[2]), fcntl.LOCK_EX)
      '')
      (pkgs.writeScriptBin "setsid" ''
        #!${pkgs.python3}/bin/python3
        import os
        import sys
        os.setsid()
        os.execvp(sys.argv[1], sys.argv[1:])
      '')
    ];
  };
  imageAgents = pkgs.symlinkJoin {
    name = "t3fleet-image-test-agents";
    paths = [ fakeT3 (pkgs.writeShellScriptBin "codex" "exit 0") (pkgs.writeShellScriptBin "claude" "exit 0") (pkgs.writeShellScriptBin "pi" "exit 0") ];
  };
  testPkgs = pkgs // { util-linux = kernelUtilities; };
  runtime = pkgs.callPackage ./runtime.nix { pkgs = testPkgs; t3Server = fakeT3; };
in
pkgs.runCommand "t3fleet-runtime-tests" { nativeBuildInputs = [ pkgs.python3 ]; } ''
  python3 - <<'PY'
  import http.server
  import json
  import os
  from pathlib import Path
  import stat
  import signal
  import socket
  import subprocess
  import sys
  import tempfile
  import threading

  class Server(http.server.BaseHTTPRequestHandler):
      version = "fixture"
      def log_message(self, *args):
          pass
      def do_GET(self):
          if self.path == "/.well-known/t3/environment":
              response = {"serverVersion": self.version, "environmentId": "env-fixture"}
          elif self.path == "/api/auth/session" and self.headers.get("Authorization") == "Bearer fixture-token":
              response = {"authenticated": True}
          else:
              self.send_response(401)
              self.end_headers()
              return
          self.send_response(200)
          self.send_header("Content-Type", "application/json")
          self.end_headers()
          self.wfile.write(json.dumps(response).encode())

  with tempfile.TemporaryDirectory() as directory:
      home = Path(directory)
      server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Server)
      thread = threading.Thread(target=server.serve_forever, daemon=True)
      thread.start()
      environment = dict(os.environ, HOME=str(home), T3FLEET_T3_PORT=str(server.server_port),
                         TEST_MINT_LOG=str(home / "mint.log"))
      def invoke():
          return subprocess.run([sys.executable, "${runtime.infoProgram}"], env=environment,
                                capture_output=True, text=True, timeout=10)
      first = invoke()
      assert first.returncode == 0, first.stderr
      assert json.loads(first.stdout) == {"token": "fixture-token", "environmentId": "env-fixture", "serverVersion": "fixture"}
      token = home / ".t3fleet/service-token"
      assert stat.S_IMODE(token.stat().st_mode) == 0o600
      assert invoke().returncode == 0
      assert (home / "mint.log").read_text().splitlines() == ["mint"]
      token.write_text("expired-token\n")
      assert invoke().returncode == 0
      assert (home / "mint.log").read_text().splitlines() == ["mint", "mint"]
      token.unlink()
      target = home / "unrelated"
      target.write_text("preserve")
      token.symlink_to(target)
      rejected = invoke()
      assert rejected.returncode != 0
      assert target.read_text() == "preserve"
      token.unlink()
      Server.version = "different"
      rejected = invoke()
      assert rejected.returncode != 0
      assert "fixture-token" not in rejected.stderr
      server.shutdown()
      thread.join()
  with tempfile.TemporaryDirectory() as directory:
      home = Path(directory)
      with socket.socket() as reserve:
          reserve.bind(("127.0.0.1", 0))
          port = reserve.getsockname()[1]
      environment = dict(os.environ, HOME=str(home), T3FLEET_T3_PORT=str(port),
                         TEST_MINT_LOG=str(home / "mint.log"))
      def handshake():
          process = subprocess.Popen(["${runtime.info}/bin/t3fleet-runtime-info"],
                                     env=environment, stdout=subprocess.PIPE,
                                     stderr=subprocess.PIPE, text=True, start_new_session=True)
          try:
              stdout, stderr = process.communicate(timeout=10)
          except subprocess.TimeoutExpired:
              os.killpg(process.pid, signal.SIGKILL)
              process.communicate()
              raise AssertionError("Runtime handshake blocked on an inherited lock")
          assert process.returncode == 0, stderr
          return json.loads(stdout)
      try:
          first = handshake()
          # The actual start wrapper launched this long-lived server. A leaked
          # info lock in it would make the second handshake time out.
          second = handshake()
          assert first == second
          assert first["environmentId"] == "env-startup-fixture"
          assert (home / "mint.log").read_text().splitlines() == ["mint"]
          settings = home / ".t3fleet/t3/userdata/settings.json"
          assert stat.S_IMODE(settings.stat().st_mode) == 0o600
          instances = json.loads(settings.read_text())["providerInstances"]
          assert {name: value["driver"] for name, value in instances.items()} == {"codex": "codex", "claudeAgent": "claudeAgent", "pi": "pi"}
          assert instances["pi"]["enabled"] is True
      finally:
          pid_file = home / "mock-server.pid"
          if pid_file.exists():
              try:
                  os.kill(int(pid_file.read_text()), signal.SIGTERM)
              except ProcessLookupError:
                  pass
  with tempfile.TemporaryDirectory() as directory:
      home = Path(directory)
      environment = dict(os.environ, HOME=str(home), PATH="${imageAgents}/bin:" + os.environ["PATH"])
      for key in ["AI_RELAY_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CODEX_ACCESS_TOKEN"]:
          environment.pop(key, None)
      def image_check():
          return subprocess.run(["${runtime.clean}/bin/t3fleet-image-check"], env=environment,
                                text=True, capture_output=True, timeout=10)
      assert image_check().returncode == 0
      environment["OPENAI_API_KEY"] = "fixture-secret"
      rejected = image_check()
      assert rejected.returncode != 0
      assert "fixture-secret" not in rejected.stderr + rejected.stdout
      environment.pop("OPENAI_API_KEY")
      state = home / ".t3fleet"
      state.mkdir()
      assert image_check().returncode != 0
      assert state.is_dir(), "Image validation must not delete runtime state"
  print("Runtime identity, startup lock lifetime, token reuse/rotation, file safety and version checks passed")
  PY
  touch "$out"
''

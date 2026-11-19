package fleet

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatchCheckoutUsesWorkerGitCredentials(t *testing.T) {
	if exec.CommandContext(t.Context(), "mv", "--version").Run() != nil {
		t.Skip("the Linux guest script requires GNU mv")
	}
	bin := t.TempDir()
	write := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nset -eu\n"+script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Replace only Git's HTTPS transport. Like a remote that challenges the
	// client, ask the real Git credential machinery for a password. Advertise
	// an empty repository after successful authentication; never use a network.
	write("git-remote-https", `
creds=$(printf 'url=%s\n\n' "$2" | git credential fill)
case "$creds" in *'password=fixture-only'*) ;; *) exit 1 ;; esac
while IFS= read -r cmd; do
  case "$cmd" in capabilities) printf 'fetch\n\n' ;; list*) printf '\n' ;; '') exit 0 ;; *) exit 1 ;; esac
done
`)
	const helper = `
test "$*" = 'auth git-credential get'
printf '%s\n' "${0##*/}" >> "$HOME/helper-used"
cat >/dev/null
printf 'username=fixture\npassword=fixture-only\n'
`
	write("gh", helper)
	write("existing-helper", helper)
	const remote = "https://github.com/acme/private.git"
	for _, tc := range []struct {
		name, remote, config, wantHelper string
	}{
		{"worker gh session", remote, "", "gh"},
		{"generic worker helper", remote, "[credential]\n helper = !existing-helper auth git-credential\n", "existing-helper"},
		{"repository helper", remote, "[credential \"https://github.com/acme/private.git\"]\n helper = !existing-helper auth git-credential\n", "existing-helper"},
		{"explicit empty helper", remote, "[credential \"https://github.com\"]\n helper =\n", ""},
		{"refusing helper", remote, "[credential]\n helper = !false\n", ""},
		{"different HTTPS host", "https://example.org/acme/private.git", "", ""},
		{"rewritten GitHub URL", remote, "[url \"https://github.com/mirror/\"]\n insteadOf = https://github.com/acme/\n", ""},
		{"SSH", "git@github.com:acme/private.git", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cfg := filepath.Join(home, ".gitconfig")
			if err := os.WriteFile(cfg, []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			run := func(name string, args ...string) ([]byte, error) {
				cmd := exec.CommandContext(t.Context(), name, args...)
				cmd.Dir = home
				cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":" + os.Getenv("PATH"), "GIT_EXEC_PATH=" + bin,
					"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + cfg, "GIT_SSH_COMMAND=false", "HTTPS_PROXY=http://127.0.0.1:1"}
				return cmd.CombinedOutput()
			}
			out, err := run("/bin/bash", "-c", dispatchCheckoutScript, "t3fleet-dispatch", strings.Repeat("1", 32), tc.remote)
			if (err == nil) != (tc.wantHelper != "") {
				t.Fatalf("checkout with %s: %v, %q", tc.name, err, out)
			}
			used, err := os.ReadFile(filepath.Join(home, "helper-used"))
			if err != nil && !os.IsNotExist(err) || strings.TrimSpace(string(used)) != tc.wantHelper {
				t.Fatalf("credential helper: got %q, want %q", used, tc.wantHelper)
			}
			if after, err := os.ReadFile(cfg); err != nil || string(after) != tc.config {
				t.Fatal("checkout changed the worker's Git configuration")
			}
			if tc.wantHelper != "" {
				path := strings.TrimSpace(string(out))
				origin, err := run("git", "-C", path, "config", "--local", "--get", "remote.origin.url")
				if err != nil || strings.TrimSpace(string(origin)) != tc.remote {
					t.Fatal("checkout did not preserve the credential-free origin")
				}
				if settings, err := run("git", "-C", path, "config", "--local", "--list"); err != nil || strings.Contains(string(settings), "credential.") || strings.Contains(string(settings), "fixture-only") {
					t.Fatal("checkout persisted credentials or a credential helper")
				}
			}
		})
	}
}

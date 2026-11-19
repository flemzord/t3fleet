package fleet

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Run the same script sent to a Linux worker against two real Git origins.
// No provider, GitHub account, credential helper, or external network is used.
func TestDispatchCheckoutClonesAndReusesOnlyItsProject(t *testing.T) {
	if runtime.GOOS != "linux" && exec.CommandContext(t.Context(), "mv", "--version").Run() != nil {
		t.Skip("the Linux guest script requires GNU mv")
	}
	home, origins := t.TempDir(), t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	run := func(dir, name string, args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), name, args...)
		cmd.Env, cmd.Dir = env, dir
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := run(dir, "git", args...); err != nil {
			t.Fatalf("git fixture: %v: %s", err, out)
		}
	}
	for _, name := range []string{"one", "two"} {
		work := t.TempDir()
		git(work, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(work, "project"), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		git(work, "add", "project")
		git(work, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "initial")
		remote := filepath.Join(origins, name+".git")
		git(work, "clone", "-q", "--bare", work, remote)
		git(remote, "update-server-info")
	}

	server := httptest.NewServer(http.FileServer(http.Dir(origins)))
	defer server.Close()
	checkout := func(key, repo string) (string, error) {
		return run(home, "/bin/bash", "-c", dispatchCheckoutScript, "t3fleet-dispatch", key, server.URL+"/"+repo+".git")
	}
	firstKey, secondKey := strings.Repeat("1", 32), strings.Repeat("2", 32)
	for _, tc := range []struct{ key, project string }{{firstKey, "one"}, {secondKey, "two"}} {
		path, err := checkout(tc.key, tc.project)
		if err != nil {
			t.Fatalf("clone: %v: %s", err, path)
		}
		contents, err := os.ReadFile(filepath.Join(path, "project"))
		if err != nil || string(contents) != tc.project {
			t.Fatalf("wrong checkout: %q: %v", contents, err)
		}
		if err := os.WriteFile(filepath.Join(path, "project"), []byte("local change"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := checkout(firstKey, "two"); err == nil {
		t.Fatal("retargeted an existing worker checkout")
	}
	server.Close() // reuse must work even if the upstream is gone
	path, err := checkout(firstKey, "one")
	if err != nil {
		t.Fatalf("reuse: %v: %s", err, path)
	}
	contents, err := os.ReadFile(filepath.Join(path, "project"))
	if err != nil || string(contents) != "local change" {
		t.Fatalf("retry lost local work: %q: %v", contents, err)
	}
}

package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type testDocument struct {
	Token string `json:"token"`
	Count int    `json:"count"`
}

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func TestSaveLoadReopen(t *testing.T) {
	s, dir := newStore(t)
	var got testDocument
	if err := s.Load(&got); !errors.Is(err, ErrEmpty) {
		t.Fatalf("empty load: %v", err)
	}
	want := testDocument{Token: "private-fixture", Count: 3}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(&got); err != nil || got != want {
		t.Fatalf("load = %#v, %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Load(&got); err != nil || got != want {
		t.Fatalf("reopened load = %#v, %v", got, err)
	}
	for path, mode := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "state.json"): 0o600, filepath.Join(dir, "lock"): 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("permissions %s: %v, %v", path, info, err)
		}
	}
}

func TestExclusiveOwnership(t *testing.T) {
	s, dir := newStore(t)
	if second, err := Open(dir); !errors.Is(err, ErrLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(testDocument{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("save after close: %v", err)
	}
	if err := s.Load(&testDocument{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("load after close: %v", err)
	}
}

func TestFailedSavePreservesDocument(t *testing.T) {
	s, dir := newStore(t)
	want := testDocument{Token: "preserved", Count: 1}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []any{make(chan int), strings.Repeat("x", MaxBytes)} {
		if err := s.Save(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid save: %v", err)
		}
	}
	var got testDocument
	if err := s.Load(&got); err != nil || got != want {
		t.Fatalf("preserved state: %#v, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".state-") {
			t.Fatal("temporary file leaked")
		}
	}
}

func TestUnsafePaths(t *testing.T) {
	for _, name := range []string{"directory", "lock", "state.json"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "state")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "target")
			if name == "directory" {
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				dir = filepath.Join(root, "linked-state")
				if err := os.Symlink(target, dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(target, []byte(`{"token":"must-not-read"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			if s, err := Open(dir); !errors.Is(err, ErrUnsafe) {
				if s != nil {
					_ = s.Close()
				}
				t.Fatalf("open symlink: %v", err)
			}
		})
	}
	if _, err := Open(""); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("empty path: %v", err)
	}
	if _, err := Open(string(filepath.Separator)); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("filesystem root: %v", err)
	}
}

func TestRejectsChangedDirectoryAndState(t *testing.T) {
	s, dir := newStore(t)
	if err := s.Save(testDocument{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "state.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(testDocument{}); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("state permissions changed: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "state.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(testDocument{}); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("directory replaced: %v", err)
	}
}

func TestInvalidAndOversizedLoad(t *testing.T) {
	s, dir := newStore(t)
	for _, content := range []string{`{"token":"secret-fixture",`, `{}` + `{}`, strings.Repeat("x", MaxBytes+1)} {
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		err := s.Load(&testDocument{})
		if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "secret-fixture") {
			t.Fatalf("invalid input: %v", err)
		}
	}
}

func TestConcurrentGoroutines(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Save(testDocument{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			if err := s.Save(testDocument{Count: i}); err != nil {
				t.Error(err)
			}
			var got testDocument
			if err := s.Load(&got); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

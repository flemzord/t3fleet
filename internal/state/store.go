// Package state stores private daemon state under a single-process ownership lock.
package state

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const MaxBytes = 16 << 20

var (
	ErrEmpty   = errors.New("state is empty")
	ErrLocked  = errors.New("state directory is already in use")
	ErrClosed  = errors.New("state store is closed")
	ErrInvalid = errors.New("state is invalid or too large")
	ErrUnsafe  = errors.New("state path is unsafe")
)

// Store owns directory until Close. The state file contains credentials and must
// not be copied into images or included in diagnostics. Store is safe for use by
// concurrent goroutines. Its lock file deliberately remains after Close.
type Store struct {
	mu   sync.Mutex
	dir  string
	info os.FileInfo
	lock *os.File
}

// Open creates a private directory or tightens its permissions. The final
// directory and existing state/lock files must not be symlinks. Parent paths
// belong to the caller (ordinary OS aliases such as macOS /var are supported).
func Open(directory string) (*Store, error) {
	if directory == "" {
		return nil, ErrUnsafe
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, ErrUnsafe
	}
	if filepath.Dir(directory) == directory {
		return nil, ErrUnsafe
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnsafe
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, err
	}
	lock, err := openPrivate(filepath.Join(directory, "lock"), syscall.O_RDWR|syscall.O_CREAT)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, err
	}
	s := &Store{dir: directory, info: info, lock: lock}
	if err := s.checkDirectory(); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.checkState(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Load decodes a complete state document. ErrEmpty means no state has been saved;
// the caller can initialize its own schema. Invalid JSON never appears in errors.
func (s *Store) Load(dst any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return ErrClosed
	}
	if err := s.checkDirectory(); err != nil {
		return err
	}
	f, err := openPrivate(filepath.Join(s.dir, "state.json"), syscall.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return ErrEmpty
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return err
	}
	if len(b) > MaxBytes || json.Unmarshal(b, dst) != nil {
		return ErrInvalid
	}
	return nil
}

// Save atomically replaces the previous document only after the new private file
// is synced. Readers never observe a partial document. A failed encode leaves
// the old state untouched.
func (s *Store) Save(value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return ErrClosed
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil || len(b)+1 > MaxBytes {
		return ErrInvalid
	}
	if err := s.checkDirectory(); err != nil {
		return err
	}
	if err := s.checkState(); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".state-*")
	if err != nil {
		return err
	}
	temporary := f.Name()
	defer func() { _ = os.Remove(temporary) }()
	if _, err = f.Write(append(b, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := s.checkDirectory(); err != nil {
		return err
	}
	if err := s.checkState(); err != nil {
		return err
	}
	if err := os.Rename(temporary, filepath.Join(s.dir, "state.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

// Close releases process ownership. Calling Close twice is harmless.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	f := s.lock
	s.lock = nil
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	return errors.Join(unlockErr, closeErr)
}

func (s *Store) checkDirectory() error {
	info, err := os.Lstat(s.dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || !os.SameFile(s.info, info) || info.Mode().Perm() != 0o700 {
		return ErrUnsafe
	}
	return nil
}

func (s *Store) checkState() error {
	info, err := os.Lstat(filepath.Join(s.dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || !ok || stat.Nlink != 1 {
		return ErrUnsafe
	}
	return nil
}

func openPrivate(path string, flags int) (*os.File, error) {
	fd, err := syscall.Open(path, flags|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, ErrUnsafe
		}
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || stat.Nlink != 1 {
		_ = f.Close()
		return nil, ErrUnsafe
	}
	return f, nil
}

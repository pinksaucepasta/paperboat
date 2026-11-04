package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const MaxSessionBytes = 2 << 20

var ErrDirectoryBusy = errors.New("SDK state directory is already in use; choose another directory or close its client")

type Memory struct {
	mu    sync.Mutex
	value Session
}

func (s *Memory) Load() (Session, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.value, nil }
func (s *Memory) Save(v Session) error   { s.mu.Lock(); defer s.mu.Unlock(); s.value = v; return nil }
func (s *Memory) Clear() error           { return s.Save(Session{}) }

// Directory exclusively owns one explicit SDK directory; it never discovers user-profile state.
type Directory struct {
	path     string
	unlock   func() error
	once     sync.Once
	closeErr error
}

func OpenDirectory(path string) (*Directory, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("SDK state path must be a real directory")
	}
	if err = securePath(path, true); err != nil {
		return nil, err
	}
	unlock, err := lockDirectory(filepath.Join(path, ".sdk.lock"))
	if err != nil {
		return nil, err
	}
	return &Directory{path: path, unlock: unlock}, nil
}

func (s *Directory) Load() (Session, error) {
	path := filepath.Join(s.path, "sdk-session.json")
	file, err := openPrivate(path)
	if errors.Is(err, os.ErrNotExist) {
		return Session{}, nil
	}
	if err != nil {
		return Session{}, err
	}
	defer file.Close()
	if err = validatePrivate(file); err != nil {
		return Session{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxSessionBytes+1))
	if err != nil {
		return Session{}, err
	}
	if len(raw) > MaxSessionBytes {
		return Session{}, errors.New("SDK session state exceeds its size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value Session
	if err = decoder.Decode(&value); err != nil {
		return Session{}, errors.New("SDK session state is invalid")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Session{}, errors.New("SDK session state contains trailing data")
	}
	return value, nil
}

func (s *Directory) Save(v Session) (resultErr error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(raw) > MaxSessionBytes {
		return errors.New("SDK session state exceeds its size limit")
	}
	file, err := os.CreateTemp(s.path, ".sdk-session-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() {
		_ = file.Close()
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err = securePath(name, false); err != nil {
		return err
	}
	if _, err = file.Write(raw); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(s.path, "sdk-session.json")); err != nil {
		return err
	}
	return syncDirectory(s.path)
}
func (s *Directory) Clear() error {
	if err := os.Remove(filepath.Join(s.path, "sdk-session.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(s.path)
}
func (s *Directory) Close() error { s.once.Do(func() { s.closeErr = s.unlock() }); return s.closeErr }

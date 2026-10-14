package sessionowner

import (
	"encoding/base64"
	"errors"
	"io"
	"os"
)

// ReadCapability accepts only the enrolled owner's private, bounded file.
func ReadCapability(d Descriptor) ([]byte, error) {
	before, err := os.Lstat(d.TokenPath())
	if err != nil {
		return nil, errors.Join(ErrInvalid, err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != 43 {
		return nil, ErrInvalid
	}
	f, err := openPrivate(d.TokenPath(), false)
	if err != nil {
		return nil, errors.Join(ErrInvalid, err)
	}
	after, statErr := f.Stat()
	body, readErr := io.ReadAll(io.LimitReader(f, 44))
	custodyErr := validatePrivate(f, d.TokenPath(), d)
	closeErr := f.Close()
	if err := errors.Join(statErr, readErr, custodyErr, closeErr); err != nil {
		return nil, errors.Join(ErrInvalid, err)
	}
	if !os.SameFile(before, after) || len(body) != 43 {
		return nil, ErrInvalid
	}
	token, err := base64.RawURLEncoding.DecodeString(string(body))
	if err != nil || len(token) != 32 {
		return nil, ErrInvalid
	}
	return token, nil
}

// LockStore is held for the full process lifetime, including IPC teardown and
// durable flush. A replacement must never open the store concurrently.
func LockStore(d Descriptor) (func() error, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	path := d.PrivateRoot() + string(os.PathSeparator) + "owner.lock"
	f, err := openPrivate(path, true)
	if err != nil {
		return nil, errors.Join(ErrInvalid, err)
	}
	if err := validatePrivate(f, path, d); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if err := lockFile(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return func() error { return errors.Join(unlockFile(f), f.Close()) }, nil
}

package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ReadEnrollmentTokenFile reads a one-time dashboard enrollment token without
// placing it in argv or environment. The caller removes it after the pairing
// request has been accepted by paperboat-server.
func ReadEnrollmentTokenFile(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", TokenFileError("its path must be absolute, without . or .. parts or repeated separators")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", TokenFileError("it does not exist or cannot be opened")
	}
	if reason := insecureEnrollmentTokenFile(path, info); reason != "" {
		return "", TokenFileError(reason)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", TokenFileError("it cannot be read")
	}
	defer clearBytes(body)
	token := strings.TrimSpace(string(body))
	if len(token) < 26 || len(token) > 256 || strings.ContainsAny(token, "\x00\r\n") {
		return "", TokenFileError("it must hold exactly the enrollment token, on one line")
	}
	return token, nil
}

// TokenFileError is the reason an enrollment token file was refused, as the user reads it. One fixed sentence hid
// which rule failed: a plain file with inherited Windows permissions was called "not a regular file".
type TokenFileError string

func (e TokenFileError) Error() string        { return "enrollment token file refused: " + string(e) }
func (e TokenFileError) Is(target error) bool { return target == ErrInvalid }

const tokenFileShape = "it must be a regular file with no links, 1 to 512 bytes long"

func ConsumeEnrollmentTokenFile(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalid
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func clearBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

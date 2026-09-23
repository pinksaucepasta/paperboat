// Package installsource identifies bytes explicitly supplied for installation.
// An official build marker is distribution policy, not proof of TUF verification.
package installsource

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/binarytarget"
)

const MaxBytes int64 = 256 << 20

const (
	Custom   = "custom"
	Official = "official"
)

var ErrInvalid = errors.New("supplied executable identity is invalid or its bytes changed")

// Source travels with the existing protected installation declaration. Hashes
// bind the administrator-approved source to staged bytes; they do not establish
// publisher authenticity. Download verification happens before invoking install.
type Source struct {
	Version          string `json:"version"`
	Platform         string `json:"platform"`
	Architecture     string `json:"architecture"`
	SHA256           string `json:"sha256"`
	Length           int64  `json:"length"`
	Distribution     string `json:"distribution"`
	AutomaticUpdates bool   `json:"automatic_updates"`
}

func Current() (string, Source, error) {
	path, err := os.Executable()
	if err != nil {
		return "", Source{}, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", Source{}, err
	}
	source, err := Inspect(path, buildinfo.Version, buildinfo.Distribution)
	return path, source, err
}

func Inspect(path, version, distribution string) (Source, error) {
	if distribution != Official {
		distribution = Custom
	}
	s := Source{Version: version, Platform: runtime.GOOS, Architecture: runtime.GOARCH, Distribution: distribution, AutomaticUpdates: distribution == Official}
	length, digest, err := digestFile(path)
	if err != nil {
		return Source{}, err
	}
	s.Length, s.SHA256 = length, digest
	if err = s.Validate(); err != nil {
		return Source{}, err
	}
	if err = binarytarget.Validate(path, s.Platform, s.Architecture); err != nil {
		return Source{}, err
	}
	return s, nil
}

func (s Source) Validate() error {
	digest, err := hex.DecodeString(s.SHA256)
	if err != nil || len(digest) != sha256.Size || s.SHA256 != strings.ToLower(s.SHA256) || s.Length < 1 || s.Length > MaxBytes || strings.TrimSpace(s.Version) != s.Version || s.Version == "" || len(s.Version) > 128 || strings.ContainsAny(s.Version, "\x00\r\n\t ") || (s.Platform != "linux" && s.Platform != "darwin" && s.Platform != "windows") || (s.Architecture != "amd64" && s.Architecture != "arm64") || (s.Distribution != Custom && s.Distribution != Official) || (s.Distribution == Custom && s.AutomaticUpdates) {
		return ErrInvalid
	}
	return nil
}

func (s Source) Verify(path string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	length, digest, err := digestFile(path)
	if err != nil || length != s.Length || digest != s.SHA256 {
		return ErrInvalid
	}
	return binarytarget.Validate(path, s.Platform, s.Architecture)
}

func digestFile(path string) (int64, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return 0, "", ErrInvalid
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 1 || before.Size() > MaxBytes {
		return 0, "", ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return 0, "", ErrInvalid
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxBytes+1))
	if err != nil || n != before.Size() {
		return 0, "", ErrInvalid
	}
	after, err := f.Stat()
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return 0, "", ErrInvalid
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

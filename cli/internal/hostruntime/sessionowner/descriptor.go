// Package sessionowner coordinates native terminal owners without owning their PTYs.
package sessionowner

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

const Schema = "paperboat.session-owner/v1"
const MaxDescriptorBytes = 64 << 10

var ErrInvalid = errors.New("session owner identity or custody is invalid")
var ErrBusy = errors.New("session owner still has active work")

// Descriptor is published by the privileged installer. Generation identifies
// the store; Source identifies the executable currently allowed to own it.
type Descriptor struct {
	Schema       string               `json:"schema"`
	Generation   string               `json:"generation"`
	Instance     string               `json:"instance"`
	Source       installsource.Source `json:"source"`
	Executable   string               `json:"executable"`
	RuntimeRoot  string               `json:"runtime_root"`
	InitialStore bool                 `json:"initial_store,omitempty"`
	Endpoint     string               `json:"endpoint"`
	UID          int                  `json:"uid"`
	GID          int                  `json:"gid"`
	OwnerSID     string               `json:"owner_sid,omitempty"`
	Workspace    string               `json:"workspace"`
	Shell        string               `json:"shell"`
}

func ValidGeneration(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16 && value == strings.ToLower(value)
}
func absolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
}
func (d Descriptor) StoreRoot() string {
	if d.InitialStore {
		return d.RuntimeRoot
	}
	return d.PrivateRoot()
}
func (d Descriptor) PrivateRoot() string {
	return filepath.Join(d.RuntimeRoot, "session-owners", d.Generation)
}
func (d Descriptor) TokenPath() string { return filepath.Join(d.PrivateRoot(), "capability") }
func (d Descriptor) Validate() error {
	if d.Schema != Schema || !ValidGeneration(d.Generation) || d.Instance == "" || len(d.Instance) > 64 || !validInstance(d.Instance) || d.Source.Validate() != nil || !absolute(d.Executable) || !absolute(d.RuntimeRoot) || !absolute(d.Workspace) || !absolute(d.Shell) || d.UID < 0 || d.GID < 0 {
		return ErrInvalid
	}
	if d.Source.Platform == "windows" {
		if d.OwnerSID == "" || d.Endpoint != `\\.\pipe\PaperboatSessionOwner-`+d.Instance+"-"+d.Generation {
			return ErrInvalid
		}
	} else if !absolute(d.Endpoint) || len(d.Endpoint) > 100 {
		return ErrInvalid
	}
	return nil
}

// Read never trusts path contents as executable authority. Native owner/DACL
// custody is checked before and through the open file; the caller separately
// verifies its registered service and the descriptor's source digest.
func Read(path string) (Descriptor, error) {
	if !absolute(path) {
		return Descriptor{}, ErrInvalid
	}
	before, err := os.Lstat(path)
	if err != nil {
		return Descriptor{}, errors.Join(ErrInvalid, err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() > MaxDescriptorBytes {
		return Descriptor{}, ErrInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return Descriptor{}, errors.Join(ErrInvalid, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(file, MaxDescriptorBytes+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if err := errors.Join(readErr, statErr, closeErr); err != nil {
		return Descriptor{}, errors.Join(ErrInvalid, err)
	}
	if len(body) > MaxDescriptorBytes || !os.SameFile(before, after) {
		return Descriptor{}, ErrInvalid
	}
	var d Descriptor
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&d) != nil || decoder.Decode(&extra) != io.EOF || d.Validate() != nil {
		return Descriptor{}, ErrInvalid
	}
	if err := validateDescriptorCustody(path, after, d); err != nil {
		return Descriptor{}, err
	}
	return d, nil
}

func validInstance(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

package configsync

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var ErrRepositoryCredentials = errors.New("repository credentials unavailable or invalid")

// repositoryFailure preserves a finite repository outcome while keeping the
// underlying cause available to diagnostics. Error text stays static because
// filesystem and decoder errors may contain private paths or credential data.
type repositoryFailure struct {
	classification error
	cause          error
}

func (e *repositoryFailure) Error() string {
	if e == nil {
		return "repository operation failed"
	}
	switch e.classification {
	case ErrRepositoryCredentials:
		return ErrRepositoryCredentials.Error()
	case ErrRepositoryUnavailable:
		return ErrRepositoryUnavailable.Error()
	default:
		return "repository operation failed"
	}
}

func (e *repositoryFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *repositoryFailure) Is(target error) bool {
	return e != nil && e.classification != nil && target == e.classification
}

func repositoryCredentialsFailure(cause error) error {
	if cause == nil {
		return ErrRepositoryCredentials
	}
	return &repositoryFailure{classification: ErrRepositoryCredentials, cause: cause}
}

func repositoryUnavailableFailure(cause error) error {
	if cause == nil {
		return ErrRepositoryUnavailable
	}
	return &repositoryFailure{classification: ErrRepositoryUnavailable, cause: cause}
}

// A missing or inaccessible user-selected credential reference is a credential
// configuration problem. Other filesystem errors indicate an operational
// failure and must not be presented as a credential rejection.
func repositoryCredentialReferenceFailure(cause error) error {
	if cause == nil {
		return ErrRepositoryCredentials
	}
	if repositoryOnlyExpectedCredentialReferenceCause(cause) {
		return repositoryCredentialsFailure(cause)
	}
	return repositoryUnavailableFailure(cause)
}

// Credential setup advice is valid only when every bounded cause leaf is a
// missing or inaccessible reference. A joined storage failure must remain an
// operational error even when it also contains one of those sentinels.
func repositoryOnlyExpectedCredentialReferenceCause(err error) bool {
	if err == nil {
		return false
	}
	pending := []error{err}
	visited := 0
	for len(pending) > 0 {
		if visited == 16 {
			return false
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		visited++
		if current == nil {
			return false
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(pending)+len(children)+visited > 16 {
				return false
			}
			pending = append(pending, children...)
			continue
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			child := wrapped.Unwrap()
			if child == nil {
				return false
			}
			pending = append(pending, child)
			continue
		}
		if !errors.Is(current, os.ErrNotExist) && !errors.Is(current, os.ErrPermission) {
			return false
		}
	}
	return visited > 0
}

// RepositoryCredentialProfile contains endpoint-owned secrets. It must never be printed.
type RepositoryCredentialProfile struct {
	Transport         string `json:"transport"`
	Auth              string `json:"auth"`
	Username          string `json:"username,omitempty"`
	Password          string `json:"password,omitempty"`
	CAFile            string `json:"ca_file,omitempty"`
	SSHKeyFile        string `json:"ssh_key_file,omitempty"`
	SSHKeyPassphrase  string `json:"ssh_key_passphrase,omitempty"`
	KnownHostsFile    string `json:"known_hosts_file,omitempty"`
	SSHAgent          bool   `json:"ssh_agent,omitempty"`
	AllowInsecureHTTP bool   `json:"allow_insecure_http,omitempty"`
}

func credentialProfilePath(root, repositoryID string) (string, error) {
	if !canonicalAbsolutePath(root) || repositoryID == "" || len(repositoryID) > 1024 {
		return "", ErrRepositoryCredentials
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", repositoryCredentialReferenceFailure(err)
	}
	if !repositoryReferencePathMatches(root, resolved) {
		return "", ErrRepositoryCredentials
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", repositoryCredentialReferenceFailure(err)
	}
	if !privateRepositoryCredentialDirectory(root, info) {
		return "", ErrRepositoryCredentials
	}
	sum := sha256.Sum256([]byte(repositoryID))
	return filepath.Join(root, "repository-"+hex.EncodeToString(sum[:])+".json"), nil
}
func validRepositoryProfile(p RepositoryCredentialProfile) bool {
	if len(p.Password) > 16384 || len(p.SSHKeyPassphrase) > 16384 || len(p.Username) > 1024 {
		return false
	}
	for _, ref := range []string{p.CAFile, p.SSHKeyFile, p.KnownHostsFile} {
		if ref != "" && !canonicalAbsolutePath(ref) {
			return false
		}
	}
	switch p.Transport {
	case "https", "http":
		return (p.Transport != "http" || p.AllowInsecureHTTP) && ((p.Auth == "anonymous" && p.Username == "" && p.Password == "") || ((p.Auth == "basic" || p.Auth == "token") && p.Username != "" && p.Password != "")) && p.SSHKeyFile == "" && !p.SSHAgent && p.SSHKeyPassphrase == "" && p.KnownHostsFile == "" && (p.Transport == "http" || !p.AllowInsecureHTTP)
	case "ssh":
		return p.Auth == "ssh" && p.KnownHostsFile != "" && ((p.SSHKeyFile != "") != p.SSHAgent) && p.Password == "" && p.CAFile == "" && !p.AllowInsecureHTTP && (!p.SSHAgent || p.SSHKeyPassphrase == "")
	case "local":
		return p.Auth == "anonymous" && p.Password == "" && p.SSHKeyPassphrase == "" && p.Username == "" && p.CAFile == "" && p.SSHKeyFile == "" && p.KnownHostsFile == "" && !p.SSHAgent && !p.AllowInsecureHTTP
	}
	return false
}
func SaveRepositoryCredentialProfile(root, repositoryID string, p RepositoryCredentialProfile) error {
	path, err := credentialProfilePath(root, repositoryID)
	if err != nil {
		return err
	}
	if !validRepositoryProfile(p) {
		return ErrRepositoryCredentials
	}
	data, err := json.Marshal(p)
	if err != nil {
		return repositoryUnavailableFailure(err)
	}
	defer clear(data)
	if err := writePrivateAtomic(path, data); err != nil {
		return repositoryUnavailableFailure(err)
	}
	return nil
}
func LoadRepositoryCredentialProfile(root, repositoryID string) (RepositoryCredentialProfile, error) {
	var p RepositoryCredentialProfile
	path, err := credentialProfilePath(root, repositoryID)
	if err != nil {
		return p, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return p, repositoryCredentialReferenceFailure(err)
	}
	if !privateControlFile(path, info) || info.Size() > 65536 {
		return p, ErrRepositoryCredentials
	}
	data, err := readRepositoryReference(path, true)
	if err != nil {
		return RepositoryCredentialProfile{}, err
	}
	defer clear(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return RepositoryCredentialProfile{}, repositoryCredentialsFailure(err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err != nil {
			return RepositoryCredentialProfile{}, repositoryCredentialsFailure(err)
		}
		return RepositoryCredentialProfile{}, ErrRepositoryCredentials
	}
	if !validRepositoryProfile(p) {
		return RepositoryCredentialProfile{}, ErrRepositoryCredentials
	}
	return p, nil
}
func DeleteRepositoryCredentialProfile(root, repositoryID string) error {
	path, err := credentialProfilePath(root, repositoryID)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return repositoryUnavailableFailure(err)
	}
	return nil
}

func (RepositoryCredentialProfile) String() string { return "repository credential profile (private)" }

// EnsureRepositoryCredentialRoot creates only the requested endpoint-private
// directory. Existing symlink paths or directories owned by others are rejected.
func EnsureRepositoryCredentialRoot(root string) error {
	if !canonicalAbsolutePath(root) {
		return ErrRepositoryCredentials
	}
	for parent := filepath.Dir(root); ; parent = filepath.Dir(parent) {
		if _, err := os.Lstat(parent); err == nil {
			resolved, err := filepath.EvalSymlinks(parent)
			if err != nil {
				return repositoryUnavailableFailure(err)
			}
			if !repositoryReferencePathMatches(parent, resolved) {
				return ErrRepositoryCredentials
			}
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return repositoryUnavailableFailure(err)
		}
		if filepath.Dir(parent) == parent {
			return ErrRepositoryCredentials
		}
	}
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		return repositoryUnavailableFailure(err)
	}
	if err := createRepositoryCredentialDirectory(root); err != nil && !errors.Is(err, os.ErrExist) {
		return repositoryUnavailableFailure(err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return repositoryUnavailableFailure(err)
	}
	if !repositoryReferencePathMatches(root, resolved) {
		return ErrRepositoryCredentials
	}
	info, err := os.Lstat(root)
	if err != nil {
		return repositoryUnavailableFailure(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrRepositoryCredentials
	}
	if err := protectRepositoryCredentialDirectory(root, info); err != nil {
		if errors.Is(err, ErrRepositoryCredentials) {
			return err
		}
		return repositoryUnavailableFailure(err)
	}
	return nil
}

// Staging directories must be owner-private at creation, just like credential
// roots. Windows os.MkdirTemp uses the elevated token's default group owner.
func newPrivateRepositoryTemporaryDirectory(parent, prefix string) (string, error) {
	if !canonicalAbsolutePath(parent) || prefix == "" || len(prefix) > 64 || strings.ContainsAny(prefix, "/\\") {
		return "", ErrRepositoryUnavailable
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", repositoryUnavailableFailure(err)
	}
	if !repositoryReferencePathMatches(parent, resolved) {
		return "", ErrRepositoryUnavailable
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return "", repositoryUnavailableFailure(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrRepositoryUnavailable
	}
	var lastErr error
	for range 16 {
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return "", repositoryUnavailableFailure(err)
		}
		path := filepath.Join(parent, prefix+hex.EncodeToString(nonce))
		if err := createRepositoryCredentialDirectory(path); errors.Is(err, os.ErrExist) {
			lastErr = err
			continue
		} else if err != nil {
			return "", repositoryUnavailableFailure(err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			removeErr := os.Remove(path)
			return "", repositoryUnavailableFailure(errors.Join(err, removeErr))
		}
		if !privateRepositoryCredentialDirectory(path, info) {
			removeErr := os.Remove(path)
			return "", repositoryUnavailableFailure(errors.Join(errors.New("private staging directory validation failed"), removeErr))
		}
		return path, nil
	}
	return "", repositoryUnavailableFailure(lastErr)
}

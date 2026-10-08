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
	if err != nil || !repositoryReferencePathMatches(root, resolved) {
		return "", ErrRepositoryCredentials
	}
	info, err := os.Lstat(root)
	if err != nil || !privateRepositoryCredentialDirectory(root, info) {
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
	if err != nil || !validRepositoryProfile(p) {
		return ErrRepositoryCredentials
	}
	data, err := json.Marshal(p)
	if err != nil {
		return ErrRepositoryCredentials
	}
	if writePrivateAtomic(path, data) != nil {
		return ErrRepositoryCredentials
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
	if err != nil || !privateControlFile(path, info) || info.Size() > 65536 {
		return p, ErrRepositoryCredentials
	}
	data, err := readRepositoryReference(path, true)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err != nil || decoder.Decode(&p) != nil || decoder.Decode(new(any)) != io.EOF || !validRepositoryProfile(p) {
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
		return ErrRepositoryCredentials
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
			if err != nil || !repositoryReferencePathMatches(parent, resolved) {
				return ErrRepositoryCredentials
			}
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return ErrRepositoryCredentials
		}
		if filepath.Dir(parent) == parent {
			return ErrRepositoryCredentials
		}
	}
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		return ErrRepositoryCredentials
	}
	if err := createRepositoryCredentialDirectory(root); err != nil && !errors.Is(err, os.ErrExist) {
		return ErrRepositoryCredentials
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || !repositoryReferencePathMatches(root, resolved) {
		return ErrRepositoryCredentials
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrRepositoryCredentials
	}
	if err := protectRepositoryCredentialDirectory(root, info); err != nil {
		return ErrRepositoryCredentials
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
	if err != nil || !repositoryReferencePathMatches(parent, resolved) {
		return "", ErrRepositoryUnavailable
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrRepositoryUnavailable
	}
	for range 16 {
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return "", ErrRepositoryUnavailable
		}
		path := filepath.Join(parent, prefix+hex.EncodeToString(nonce))
		if err := createRepositoryCredentialDirectory(path); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", ErrRepositoryUnavailable
		}
		info, err := os.Lstat(path)
		if err != nil || !privateRepositoryCredentialDirectory(path, info) {
			os.Remove(path)
			return "", ErrRepositoryUnavailable
		}
		return path, nil
	}
	return "", ErrRepositoryUnavailable
}

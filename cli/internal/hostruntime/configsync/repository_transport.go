package configsync

import (
	"context"
	"crypto/x509"
	"errors"
	sshagent "github.com/xanzy/ssh-agent"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// NormalizeRepositoryEndpoint accepts credential-free, explicit Git endpoints.
func NormalizeRepositoryEndpoint(raw string) (string, string, error) {
	if raw == "" || len(raw) > 4096 || strings.TrimSpace(raw) != raw || strings.ContainsFunc(raw, unicode.IsControl) {
		return "", "", ErrGitRepositoryInvalid
	}
	windowsDrive := len(raw) > 2 && ((raw[0] >= 'A' && raw[0] <= 'Z') || (raw[0] >= 'a' && raw[0] <= 'z')) && raw[1] == ':' && (raw[2] == '\\' || raw[2] == '/')
	if strings.HasPrefix(raw, "/") || windowsDrive || strings.HasPrefix(raw, "\\\\") {
		if strings.ContainsAny(raw, "?#") || endpointTraversal(strings.ReplaceAll(raw, "\\", "/")) {
			return "", "", ErrGitRepositoryInvalid
		}
		return raw, "local", nil
	}
	if !strings.Contains(raw, "://") {
		ep, err := transport.NewEndpoint(raw)
		if err != nil || ep.Protocol != "ssh" || ep.Host == "" || ep.Path == "" || strings.ContainsAny(raw, "?#\\") || endpointTraversal(ep.Path) {
			return "", "", ErrGitRepositoryInvalid
		}
		return raw, "ssh", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || endpointTraversal(u.Path) {
		return "", "", ErrGitRepositoryInvalid
	}
	switch u.Scheme {
	case "https", "http", "ssh":
		if u.Hostname() == "" || u.Path == "" || u.Path == "/" || strings.Contains(u.Path, "\\") {
			return "", "", ErrGitRepositoryInvalid
		}
		if u.User != nil {
			if u.Scheme != "ssh" || u.User.Username() == "" {
				return "", "", ErrGitRepositoryInvalid
			}
			if _, has := u.User.Password(); has {
				return "", "", ErrGitRepositoryInvalid
			}
		}
		return u.String(), u.Scheme, nil
	case "file":
		if (u.Host != "" && u.Host != "localhost") || u.User != nil || !strings.HasPrefix(u.Path, "/") {
			return "", "", ErrGitRepositoryInvalid
		}
		return u.String(), "local", nil
	}
	return "", "", ErrGitRepositoryInvalid
}
func endpointTraversal(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == ".." || part == "." {
			return true
		}
	}
	return false
}

// nativeRepositoryPath resolves only explicit syntax on the owning OS. A path
// naming another OS is unavailable rather than treated as a relative local path.
func nativeRepositoryPath(endpoint string) (string, error) {
	path := endpoint
	if strings.HasPrefix(endpoint, "file://") {
		u, err := url.Parse(endpoint)
		if err != nil {
			return "", ErrGitRepositoryInvalid
		}
		path = filepath.FromSlash(u.Path)
		if len(path) > 3 && (path[0] == '/' || path[0] == '\\') && path[2] == ':' {
			path = path[1:]
		}
	}
	if !canonicalAbsolutePath(path) {
		return "", ErrRepositoryUnavailable
	}
	return path, nil
}

var installRepositoryTransports sync.Once

func initRepositoryTransports() {
	installRepositoryTransports.Do(func() {
		// Redirects can change the exact granted resource or expose Authorization.
		hc := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		client.InstallProtocol("https", githttp.NewClient(hc))
		client.InstallProtocol("http", githttp.NewClient(hc))
		client.InstallProtocol("file", server.NewClient(server.DefaultLoader))
		client.InstallProtocol("ssh", repositorySSHTransport{})
	})
}

type repositoryTransportAccess struct {
	auth   transport.AuthMethod
	ca     []byte
	closer io.Closer
}

func (r *GitRepository) transportAccess(ctx context.Context, a RepositoryAccess) (repositoryTransportAccess, error) {
	return resolveRepositoryTransportAccess(ctx, r.credentialRoot, a)
}

func resolveRepositoryTransportAccess(ctx context.Context, credentialRoot string, a RepositoryAccess) (repositoryTransportAccess, error) {
	var result repositoryTransportAccess
	if a.Password != "" {
		if a.Transport != "https" || a.Username != "x-access-token" {
			return result, ErrGitRepositoryInvalid
		}
		result.auth = &githttp.BasicAuth{Username: a.Username, Password: a.Password}
		return result, nil
	}
	if a.Transport == "local" {
		return result, nil
	}
	p, err := LoadRepositoryCredentialProfile(credentialRoot, a.RepositoryID)
	if err != nil || p.Transport != a.Transport {
		return result, ErrRepositoryCredentials
	}
	if p.CAFile != "" {
		data, err := readRepositoryReference(p.CAFile, false)
		if err != nil {
			return result, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return result, ErrRepositoryCredentials
		}
		result.ca = data
	}
	switch a.Transport {
	case "http", "https":
		if p.Auth != "anonymous" {
			result.auth = &githttp.BasicAuth{Username: p.Username, Password: p.Password}
		}
	case "ssh":
		if _, err := readRepositoryReference(p.KnownHostsFile, false); err != nil {
			return result, err
		}
		db, err := gitssh.NewKnownHostsDb(p.KnownHostsFile)
		if err != nil {
			return result, ErrRepositoryCredentials
		}
		endpoint, err := transport.NewEndpoint(a.CloneURL)
		if err != nil {
			return result, ErrGitRepositoryInvalid
		}
		port := endpoint.Port
		if port == 0 {
			port = gitssh.DefaultPort
		}
		algorithms := db.HostKeyAlgorithms(net.JoinHostPort(endpoint.Host, strconv.Itoa(port)))
		if len(algorithms) == 0 {
			return result, ErrRepositoryCredentials
		}
		cb := db.HostKeyCallback()
		user := endpoint.User
		if p.Username != "" {
			if user != "" && user != p.Username {
				return result, ErrRepositoryCredentials
			}
			user = p.Username
		}
		if user == "" {
			user = "git"
		}
		if p.SSHAgent {
			agent, conn, err := sshagent.New()
			if err != nil {
				return result, ErrRepositoryCredentials
			}
			auth := &gitssh.PublicKeysCallback{User: user, Callback: agent.Signers}
			auth.HostKeyCallback = cb
			auth.HostKeyAlgorithms = algorithms
			result.closer = newRepositoryContextCloser(ctx, conn)
			result.auth = &repositorySSHAuth{AuthMethod: auth, ctx: ctx}
		} else {
			key, err := readRepositoryReference(p.SSHKeyFile, true)
			if err != nil {
				return result, err
			}
			auth, err := gitssh.NewPublicKeys(user, key, p.SSHKeyPassphrase)
			if err != nil {
				return result, ErrRepositoryCredentials
			}
			auth.HostKeyCallback = cb
			auth.HostKeyAlgorithms = algorithms
			result.auth = &repositorySSHAuth{AuthMethod: auth, ctx: ctx}
		}
	default:
		return result, ErrGitRepositoryInvalid
	}
	return result, nil
}
func readRepositoryReference(path string, private bool) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !repositoryReferencePathMatches(path, resolved) {
		return nil, ErrRepositoryCredentials
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1048576 || !secureRepositoryReference(path, info, private) {
		return nil, ErrRepositoryCredentials
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrRepositoryCredentials
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, ErrRepositoryCredentials
	}
	data, err := io.ReadAll(io.LimitReader(file, 1048577))
	if err != nil || len(data) > 1048576 {
		return nil, ErrRepositoryCredentials
	}
	return data, nil
}

var ErrRepositoryUnavailable = errors.New("config repository unavailable")

func (a repositoryTransportAccess) close() {
	if a.closer != nil {
		a.closer.Close()
	}
}

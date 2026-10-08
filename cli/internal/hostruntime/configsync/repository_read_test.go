package configsync

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func readGrant(url, kind string) RepositoryReadAccess {
	return RepositoryReadAccess{RepositoryID: "repo", CloneURL: url, Transport: kind, Branch: "master", Capability: "repository_contents_read", ExpiresAt: time.Now().Add(5 * time.Minute)}
}
func setSharedSource(t *testing.T, bare string, data []byte) {
	t.Helper()
	r, err := git.PlainOpen(filepath.Join(filepath.Dir(bare), "seed"))
	if err != nil {
		t.Fatal(err)
	}
	w, _ := r.Worktree()
	path := filepath.Join(filepath.Dir(bare), "seed", filepath.FromSlash(SharedSourceConfigPath))
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(path, data, 0600)
	w.Add(SharedSourceConfigPath)
	if _, err = w.Commit("source", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@invalid", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err = r.Push(&git.PushOptions{}); err != nil {
		t.Fatal(err)
	}
}
func assertBootstrapRead(t *testing.T, url, kind, credentials string) {
	t.Helper()
	root := t.TempDir()
	source, revision, err := ReadSharedRepositoryConfig(context.Background(), root, credentials, readGrant(url, kind), DefaultSourceConfigLimits())
	if err != nil || !plumbing.IsHash(revision) {
		t.Fatalf("read revision=%q error=%v", revision, err)
	}
	if source.Version != nil && *source.Version != 1 {
		t.Fatal("incorrect source")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("read cache retained")
	}
}
func TestRepositoryReadLocalOptionalInvalidAndAuthority(t *testing.T) {
	bare, target := transportFixture(t)
	t.Setenv("PATH", "")
	assertBootstrapRead(t, bare, "local", "")
	setSharedSource(t, bare, []byte(`"version" = 1
"enabled" = true
`))
	before, _ := target.Head()
	root := t.TempDir()
	source, rev, err := ReadSharedRepositoryConfig(context.Background(), root, "", readGrant(bare, "local"), DefaultSourceConfigLimits())
	if err != nil || source.Enabled == nil || !*source.Enabled || rev != before.Hash().String() {
		t.Fatal("source read", err)
	}
	after, _ := target.Head()
	if before.Hash() != after.Hash() {
		t.Fatal("read mutated remote")
	}
	a := readGrant(bare, "local")
	a.Capability = "repository_contents_write"
	if _, _, err = ReadSharedRepositoryConfig(context.Background(), root, "", a, DefaultSourceConfigLimits()); !errors.Is(err, ErrAuthorization) {
		t.Fatal("write grant accepted")
	}
	a = readGrant(bare, "local")
	a.ExpiresAt = time.Now().Add(-time.Second)
	if _, _, err = ReadSharedRepositoryConfig(context.Background(), root, "", a, DefaultSourceConfigLimits()); !errors.Is(err, ErrAuthorization) {
		t.Fatal("expired grant accepted")
	}
	setSharedSource(t, bare, []byte(`"unexpected" = true
`))
	if _, _, err = ReadSharedRepositoryConfig(context.Background(), root, "", readGrant(bare, "local"), DefaultSourceConfigLimits()); !errors.Is(err, ErrSourceConfigInvalid) {
		t.Fatal("invalid source accepted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err = ReadSharedRepositoryConfig(ctx, root, "", readGrant(bare, "local"), DefaultSourceConfigLimits()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("failed read retained cache")
	}
}
func TestRepositoryReadHTTPS(t *testing.T) {
	bare, _ := transportFixture(t)
	setSharedSource(t, bare, []byte(`"version" = 1
`))
	srv := httptest.NewTLSServer(smartGitHandler(t, bare, "reader", "read-secret"))
	defer srv.Close()
	root := t.TempDir()
	EnsureRepositoryCredentialRoot(root)
	cert, err := x509.ParseCertificate(srv.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(root, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600)
	if err := SaveRepositoryCredentialProfile(root, "repo", RepositoryCredentialProfile{Transport: "https", Auth: "basic", Username: "reader", Password: "read-secret", CAFile: ca}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	assertBootstrapRead(t, srv.URL+"/repo.git", "https", root)
}
func TestRepositoryReadPackLimits(t *testing.T) {
	observer := &repositoryReadObserver{}
	if !errors.Is(observer.OnHeader(repositoryReadMaxObjects+1), ErrRepositoryReadLimit) {
		t.Fatal("object count unbounded")
	}
	if !errors.Is(observer.OnInflatedObjectHeader(plumbing.BlobObject, repositoryReadMaxObjectBytes+1, 0), ErrRepositoryReadLimit) {
		t.Fatal("inflated object unbounded")
	}
}

func TestRepositoryReadRejectsOversizedInflatedPack(t *testing.T) {
	bare, _ := transportFixture(t)
	setSharedSource(t, bare, bytes.Repeat([]byte(" "), int(repositoryReadMaxObjectBytes)+1))
	t.Setenv("PATH", "")
	root := t.TempDir()
	_, _, err := ReadSharedRepositoryConfig(context.Background(), root, "", readGrant(bare, "local"), DefaultSourceConfigLimits())
	if !errors.Is(err, ErrRepositoryReadLimit) {
		t.Fatal("oversized inflated object accepted", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("failed oversized read retained cache")
	}
}

func TestRepositoryReadMachineGrantReadWriteIdentity(t *testing.T) {
	bare, _ := transportFixture(t)
	t.Setenv("PATH", "")
	access := RepositoryAccess{RepositoryID: "repo", AssignmentID: "assignment", EnvironmentID: "environment", MachineID: "machine", CloneURL: bare, Branch: "master", Transport: "local", Capability: "repository_contents_write", ExpiresAt: time.Now().Add(time.Minute)}
	if _, revision, err := ReadSharedAuthorizedRepositoryConfig(context.Background(), t.TempDir(), "", access, DefaultSourceConfigLimits()); err != nil || !plumbing.IsHash(revision) {
		t.Fatal("actual write capability could not read source", err)
	}
	access.MachineID = ""
	if _, _, err := ReadSharedAuthorizedRepositoryConfig(context.Background(), t.TempDir(), "", access, DefaultSourceConfigLimits()); !errors.Is(err, ErrAuthorization) {
		t.Fatal("unbound machine grant accepted", err)
	}
}

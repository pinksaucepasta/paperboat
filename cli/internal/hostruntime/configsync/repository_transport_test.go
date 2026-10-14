package configsync

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func transportFixture(t *testing.T) (string, *git.Repository) {
	t.Helper()
	initRepositoryTransports()
	root := t.TempDir()
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(root, "remote.git")
	target, err := git.PlainInit(bare, true)
	if err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(root, "seed")
	r, err := git.PlainInit(seed, false)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(seed, ".pbinclude"), []byte("settings\n"), 0600)
	w, _ := r.Worktree()
	w.Add(".pbinclude")
	_, err = w.Commit("seed", &git.CommitOptions{Author: &object.Signature{Name: "Paperboat", Email: "config@paperboat.invalid", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}})
	if err := r.Push(&git.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatal(err)
	}
	return bare, target
}
func smartGitHandler(t *testing.T, bare string, user, password string) http.Handler {
	t.Helper()
	ep, _ := transport.NewEndpoint(bare)
	srv := server.NewServer(server.DefaultLoader)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != password {
			w.Header().Set("WWW-Authenticate", "Basic realm=git")
			w.WriteHeader(401)
			return
		}
		service := r.URL.Query().Get("service")
		if service == "" {
			service = filepath.Base(r.URL.Path)
		}
		if service != "git-upload-pack" && service != "git-receive-pack" {
			w.WriteHeader(404)
			return
		}
		if service == "git-upload-pack" {
			session, err := srv.NewUploadPackSession(ep, nil)
			if err != nil {
				w.WriteHeader(500)
				return
			}
			defer session.Close()
			if r.Method == "GET" {
				w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
				pktline.NewEncoder(w).EncodeString("# service=git-upload-pack\n")
				pktline.NewEncoder(w).Flush()
				ar, err := session.AdvertisedReferencesContext(r.Context())
				if err != nil {
					t.Error(err)
					return
				}
				ar.Encode(w)
				return
			}
			req := packp.NewUploadPackRequest()
			if err := req.Decode(r.Body); err != nil {
				t.Error(err)
				return
			}
			resp, err := session.UploadPack(r.Context(), req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Close()
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			resp.Encode(w)
			return
		}
		session, err := srv.NewReceivePackSession(ep, nil)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		defer session.Close()
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			pktline.NewEncoder(w).EncodeString("# service=git-receive-pack\n")
			pktline.NewEncoder(w).Flush()
			ar, _ := session.AdvertisedReferencesContext(r.Context())
			ar.Encode(w)
			return
		}
		req := packp.NewReferenceUpdateRequest()
		if err := req.Decode(r.Body); err != nil {
			t.Error(err)
			return
		}
		resp, err := session.ReceivePack(r.Context(), req)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		resp.Encode(w)
	})
}
func newTransportRepository(t *testing.T, endpoint, kind, credentials string) *GitRepository {
	t.Helper()
	r, err := NewGitRepository(GitRepositoryConfig{Root: filepath.Join(t.TempDir(), "checkout"), CredentialRoot: credentials, Access: staticAccessSource{RepositoryAccess{RepositoryID: "repo", CloneURL: endpoint, PublishURL: endpoint, Branch: "master", Transport: kind, Capability: "repository_contents_write", ExpiresAt: time.Now().Add(time.Minute)}}, Reconciler: committingReconciler{}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestCustomHTTPSNoGitTrustAuthPublish(t *testing.T) {
	bare, _ := transportFixture(t)
	srv := httptest.NewTLSServer(smartGitHandler(t, bare, "alice", "private-password"))
	defer srv.Close()
	t.Setenv("PATH", "")
	root := t.TempDir()
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(root, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600)
	profile := RepositoryCredentialProfile{Transport: "https", Auth: "basic", Username: "alice", Password: "private-password", CAFile: ca}
	if err := SaveRepositoryCredentialProfile(root, "repo", profile); err != nil {
		t.Fatal(err)
	}
	repo := newTransportRepository(t, srv.URL+"/repo.git", "https", root)
	snap, err := repo.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := repo.Reconcile(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	result, err := repo.Publish(context.Background(), prepared, 1)
	if err != nil || !result.Landed {
		t.Fatalf("push landed=%v err=%v", result.Landed, err)
	}
	landed, _, err := repo.ObserveCommit(context.Background(), prepared.CommitID)
	if err != nil || !landed {
		t.Fatal("observation failed", err)
	}
	data, _ := os.ReadFile(filepath.Join(repo.root, ".git", "config"))
	if strings.Contains(string(data), profile.Password) {
		t.Fatal("credential persisted in URL")
	}
	profile.CAFile = ""
	SaveRepositoryCredentialProfile(root, "repo", profile)
	if _, err := repo.Fetch(context.Background()); err == nil {
		t.Fatal("untrusted TLS accepted")
	}
	profile.CAFile = ca
	profile.Password = "wrong"
	SaveRepositoryCredentialProfile(root, "repo", profile)
	if _, err := repo.Fetch(context.Background()); err == nil {
		t.Fatal("invalid credentials accepted")
	}
}
func TestRepositoryHTTPOptInRedirectCredentials(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		t.Fatal(err)
	}
	p := RepositoryCredentialProfile{Transport: "http", Auth: "basic", Username: "user", Password: "secret"}
	if SaveRepositoryCredentialProfile(root, "repo", p) == nil {
		t.Fatal("HTTP without opt-in accepted")
	}
	p.AllowInsecureHTTP = true
	SaveRepositoryCredentialProfile(root, "repo", p)
	contacted := false
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted = true; io.WriteString(w, "not a repository") }))
	defer dst.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dst.URL+"/other.git", 302) }))
	defer src.Close()
	repo := newTransportRepository(t, src.URL+"/repo.git", "http", root)
	if _, err := repo.Fetch(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
	if contacted {
		t.Fatal("redirect endpoint contacted")
	}
}
func TestCredentialProfilesPrivateIdentityAndInvalid(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		t.Fatal(err)
	}
	p := RepositoryCredentialProfile{Transport: "https", Auth: "token", Username: "token", Password: "secret"}
	if err := SaveRepositoryCredentialProfile(root, "a", p); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRepositoryCredentialProfile(root, "a")
	if err != nil || got.Password != p.Password {
		t.Fatal("roundtrip")
	}
	if _, err := LoadRepositoryCredentialProfile(root, "b"); !errors.Is(err, ErrRepositoryCredentials) {
		t.Fatal("cross repository credentials")
	}
	path, _ := credentialProfilePath(root, "a")
	os.Chmod(path, 0644)
	if _, err := LoadRepositoryCredentialProfile(root, "a"); err == nil {
		t.Fatal("public secret file accepted")
	}
	DeleteRepositoryCredentialProfile(root, "a")
	if _, err := LoadRepositoryCredentialProfile(root, "a"); err == nil {
		t.Fatal("deleted profile available")
	}
}
func TestLocalMissingUnsafeAndStalePublication(t *testing.T) {
	bare, target := transportFixture(t)
	repo := newTransportRepository(t, bare, "local", "")
	snap, err := repo.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := repo.Reconcile(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.Publish(ctx, prepared, 1); err == nil {
		t.Fatal("cancel accepted")
	}
	ref, _ := target.Reference(plumbing.NewBranchReferenceName("master"), true)
	if ref.Hash().String() != snap.Revision {
		t.Fatal("cancel mutated reference")
	}
	prepared.ExpectedRemoteRevision = strings.Repeat("1", 40)
	if _, err := repo.Publish(context.Background(), prepared, 1); !errors.Is(err, ErrRemoteRevisionChanged) {
		t.Fatal("stale publication accepted", err)
	}
	missing := newTransportRepository(t, filepath.Join(t.TempDir(), "missing.git"), "local", "")
	if _, err := missing.Fetch(context.Background()); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatal("missing repository", err)
	}
	nonbare := filepath.Join(t.TempDir(), "worktree")
	git.PlainInit(nonbare, false)
	if _, err := openLocalRepository(nonbare); err == nil {
		t.Fatal("nonbare push target accepted")
	}
}

func TestConcurrentRepositoryHTTPSProfilesIsolated(t *testing.T) {
	type endpoint struct {
		repo *GitRepository
		srv  *httptest.Server
	}
	endpoints := make([]endpoint, 2)
	for i := range endpoints {
		bare, _ := transportFixture(t)
		user := []string{"alice", "bob"}[i]
		password := []string{"alice-secret", "bob-secret"}[i]
		srv := uniqueTLSServer(t, smartGitHandler(t, bare, user, password))
		defer srv.Close()
		root := t.TempDir()
		if err := EnsureRepositoryCredentialRoot(root); err != nil {
			t.Fatal(err)
		}
		ca := filepath.Join(root, "ca.pem")
		os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600)
		if err := SaveRepositoryCredentialProfile(root, "repo", RepositoryCredentialProfile{Transport: "https", Auth: "basic", Username: user, Password: password, CAFile: ca}); err != nil {
			t.Fatal(err)
		}
		endpoints[i] = endpoint{repo: newTransportRepository(t, srv.URL+"/repo.git", "https", root), srv: srv}
	}
	failures := make(chan error, 2)
	for _, e := range endpoints {
		go func(r *GitRepository) { _, err := r.Fetch(context.Background()); failures <- err }(e.repo)
	}
	for range endpoints {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
}

type cancelAfterChecks struct {
	context.Context
	checks int
	limit  int
}

func (c *cancelAfterChecks) Err() error {
	c.checks++
	if c.checks >= c.limit {
		return context.Canceled
	}
	return nil
}
func TestLocalInterruptedCopyLocksAndExactBranch(t *testing.T) {
	bare, target := transportFixture(t)
	repo := newTransportRepository(t, bare, "local", "")
	snap, err := repo.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := repo.Reconcile(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	source, err := git.PlainOpen(repo.root)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := repo.access.RepositoryAccess(context.Background())
	ctx := &cancelAfterChecks{Context: context.Background(), limit: 5}
	if err := copyLocalObjects(ctx, source, target, plumbing.NewHash(prepared.CommitID), plumbing.NewHash(snap.Revision), bare); !errors.Is(err, context.Canceled) {
		t.Fatal("copy cancellation missing", err)
	}
	ref, _ := target.Reference(plumbing.NewBranchReferenceName("master"), true)
	if ref.Hash().String() != snap.Revision {
		t.Fatal("interruption mutated branch")
	}
	if target.Storer.HasEncodedObject(plumbing.NewHash(prepared.CommitID)) != nil {
		t.Fatal("test did not interrupt after partial object copy")
	}
	branchLock := filepath.Join(bare, "refs", "heads", "master.lock")
	if _, err := os.Stat(branchLock); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned lock retained")
	}
	os.WriteFile(branchLock, []byte("other writer"), 0600)
	if _, err := publishLocal(context.Background(), source, a, prepared); err == nil {
		t.Fatal("concurrent writer lock ignored")
	}
	data, _ := os.ReadFile(branchLock)
	if string(data) != "other writer" {
		t.Fatal("other writer lock altered")
	}
	os.Remove(branchLock)
	other := plumbing.NewHashReference(plumbing.NewBranchReferenceName("other"), plumbing.NewHash(snap.Revision))
	target.Storer.SetReference(other)
	result, err := publishLocal(context.Background(), source, a, prepared)
	if err != nil || !result.Landed {
		t.Fatal("recovery failed", err)
	}
	untouched, _ := target.Reference(other.Name(), true)
	if untouched.Hash() != other.Hash() {
		t.Fatal("unrelated branch changed")
	}
}

func TestRepositoryEndpointValidation(t *testing.T) {
	for _, raw := range []string{"https://user:secret@example.test/repo.git", "http://example.test/repo.git?token=secret", "ssh://git:secret@example.test/repo.git", "ssh://host/../repo.git", "https://host/a%2f..%2frepo.git", "git://host/repo.git", "file://other/home/repo.git", "relative/repo.git"} {
		if _, _, err := NormalizeRepositoryEndpoint(raw); err == nil {
			t.Errorf("invalid endpoint accepted: %s", raw)
		}
	}
	for _, raw := range []string{"https://example.test/repo.git", "http://127.0.0.1:1234/repo.git", "ssh://git@192.168.1.2:2222/repo.git", "git@192.168.1.2:repo.git", "C:\\Users\\User\\repo.git", "\\\\host\\share\\repo.git", "file://localhost/srv/repo.git", filepath.Join(t.TempDir(), "repo.git")} {
		if _, _, err := NormalizeRepositoryEndpoint(raw); err != nil {
			t.Errorf("valid endpoint rejected: %s", raw)
		}
	}
}

func TestCustomHTTPSAmbiguousPushObservedWithoutRetry(t *testing.T) {
	bare, _ := transportFixture(t)
	handler := smartGitHandler(t, bare, "alice", "secret")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			handler.ServeHTTP(httptest.NewRecorder(), r)
			// The branch landed, but the peer disappears before any push response.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	root := t.TempDir()
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(root, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600)
	SaveRepositoryCredentialProfile(root, "repo", RepositoryCredentialProfile{Transport: "https", Auth: "basic", Username: "alice", Password: "secret", CAFile: ca})
	repo := newTransportRepository(t, srv.URL+"/repo.git", "https", root)
	snap, err := repo.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := repo.Reconcile(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	result, err := repo.Publish(context.Background(), prepared, 1)
	if err == nil || !result.Uncertain || result.Landed {
		t.Fatalf("missing uncertain result: %+v %v", result, err)
	}
	landed, revision, err := repo.ObserveCommit(context.Background(), prepared.CommitID)
	if err != nil || !landed || revision != prepared.CommitID {
		t.Fatal("landed uncertain push not observed", err)
	}
}

func TestRepositoryExpiredGrantAndFailedCloneRecovery(t *testing.T) {
	bare, _ := transportFixture(t)
	handler := smartGitHandler(t, bare, "alice", "secret")
	srv := httptest.NewTLSServer(handler)
	defer srv.Close()
	root := t.TempDir()
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(root, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600)
	p := RepositoryCredentialProfile{Transport: "https", Auth: "basic", Username: "alice", Password: "wrong", CAFile: ca}
	SaveRepositoryCredentialProfile(root, "repo", p)
	repo := newTransportRepository(t, srv.URL+"/repo.git", "https", root)
	if _, err := repo.Fetch(context.Background()); err == nil {
		t.Fatal("wrong credential accepted")
	}
	if _, err := os.Stat(repo.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed clone retained unusable checkout")
	}
	stages, _ := filepath.Glob(filepath.Join(filepath.Dir(repo.root), ".pb-config-clone-*"))
	if len(stages) != 0 {
		t.Fatal("failed clone staging retained")
	}
	p.Password = "secret"
	SaveRepositoryCredentialProfile(root, "repo", p)
	if _, err := repo.Fetch(context.Background()); err != nil {
		t.Fatal("auth repair did not recover clone", err)
	}
	a, _ := repo.access.RepositoryAccess(context.Background())
	a.ExpiresAt = time.Now().Add(-time.Second)
	repo.access = staticAccessSource{access: a}
	if _, err := repo.Fetch(context.Background()); !errors.Is(err, ErrAuthorization) {
		t.Fatal("expired grant did not fence available credentials", err)
	}
}

func uniqueTLSServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	return server
}

func TestLocalMountUnavailableAndRecoveryPreservesCheckout(t *testing.T) {
	bare, _ := transportFixture(t)
	repo := newTransportRepository(t, bare, "local", "")
	snap, err := repo.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bare, bare+".offline"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Fetch(context.Background()); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatal("offline mount not unavailable", err)
	}
	if data, err := os.ReadFile(filepath.Join(repo.root, ".pbinclude")); err != nil || string(data) != "settings\n" {
		t.Fatal("unavailable mount modified checkout")
	}
	if err := os.Rename(bare+".offline", bare); err != nil {
		t.Fatal(err)
	}
	recovered, err := repo.Fetch(context.Background())
	if err != nil || recovered.Revision != snap.Revision {
		t.Fatal("mount recovery failed", err)
	}
	objects := filepath.Join(bare, "objects")
	if err := os.Rename(objects, objects+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(objects+".real", objects); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Fetch(context.Background()); err == nil {
		t.Fatal("symlink object escape allowed")
	}
}

func TestCredentialProfileStrictDataAndRootProtection(t *testing.T) {
	root := filepath.Join(t.TempDir(), "credentials")
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		t.Fatal(err)
	}
	p := RepositoryCredentialProfile{Transport: "https", Auth: "basic", Username: "alice", Password: "secret"}
	if err := SaveRepositoryCredentialProfile(root, "repo", p); err != nil {
		t.Fatal(err)
	}
	path, _ := credentialProfilePath(root, "repo")
	data, _ := os.ReadFile(path)
	for _, malformed := range [][]byte{append(append([]byte{}, data...), []byte("{}")...), []byte(`{"transport":"https","auth":"anonymous","password":"hidden"}`), []byte(`{"transport":"ssh","auth":"ssh","ssh_agent":true,"ssh_key_passphrase":"hidden","known_hosts_file":"/known_hosts"}`)} {
		if err := writePrivateAtomic(path, malformed); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRepositoryCredentialProfile(root, "repo"); err == nil {
			t.Fatal("mixed or trailing credential profile accepted")
		}
	}
}

func TestLocalFileURLNoGitPublish(t *testing.T) {
	bare, _ := transportFixture(t)
	t.Setenv("PATH", "")
	repo := newTransportRepository(t, "file://localhost"+filepath.ToSlash(bare), "local", "")
	snap, err := repo.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := repo.Reconcile(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	result, err := repo.Publish(context.Background(), prepared, 1)
	if err != nil || !result.Landed {
		t.Fatal("local file URL publish failed", err)
	}
}

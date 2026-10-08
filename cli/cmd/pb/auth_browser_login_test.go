package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
)

func browserLoginFixture(t *testing.T, issuer string) (string, config.ProfileStore) {
	t.Helper()
	dir := t.TempDir()
	isolateCommandCredentialLocation(t, dir)
	path := filepath.Join(dir, "config.json")
	c, e := config.Load(path)
	if e != nil {
		t.Fatal(e)
	}
	c.ServerURL = issuer
	c.Auth.AllowFileFallback = true
	c.Auth.ProfileDir = filepath.Join(dir, "profiles")
	if e = c.Save(); e != nil {
		t.Fatal(e)
	}
	s, e := config.ProfileStoreFor(c)
	if e != nil {
		t.Fatal(e)
	}
	return path, s
}
func runBrowserLogin(t *testing.T, ctx context.Context, path string, args ...string) (string, error) {
	t.Helper()
	r := newRootCommand()
	var b bytes.Buffer
	r.SetOut(&b)
	r.SetErr(&b)
	r.SetArgs(append([]string{"--config", path}, args...))
	e := r.ExecuteContext(ctx)
	return b.String(), e
}
func TestBrowserLoginAliasesApprovalAndRecovery(t *testing.T) {
	code := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	polls := 0
	authorizations := 0
	cancelled := false
	failMe := true
	failEnrollment := true
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/device/authorize":
			authorizations++
			writeAPIData(t, w, map[string]any{"device_code": code, "user_code": "ABCD-EFGH", "verification_uri": srv.URL + "/cli/authorize", "verification_uri_complete": srv.URL + "/cli/authorize?code=ABCD-EFGH", "expires_in": 600, "interval": 1})
		case "/v1/auth/device/token":
			polls++
			if polls == 1 {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "authorization_pending"}})
				return
			}
			writeAPIData(t, w, map[string]any{"access_token": "test-access", "refresh_token": "test-refresh", "token_type": "Bearer", "expires_in": 3600, "cli_client_session_id": "cls_browser"})
		case "/v1/me":
			if failMe {
				failMe = false
				w.WriteHeader(503)
				return
			}
			if r.Header.Get("Authorization") != "Bearer test-access" {
				t.Error("missing credential")
			}
			writeAPIData(t, w, map[string]string{"id": "usr_browser", "email": "browser@example.test", "status": "active"})
		case "/v1/e2ee/bootstrap":
			if failEnrollment {
				failEnrollment = false
				w.WriteHeader(503)
				return
			}
			acceptBrowserPeerEnrollment(t, w, r, "usr_browser", "cls_browser")
		case "/v1/auth/device/cancel":
			cancelled = true
			w.WriteHeader(204)
		case "/v1/auth/token/revoke":
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	oldHTTP, oldWait, oldBrowser := loginHTTPClient, loginWait, openBrowser
	t.Cleanup(func() { loginHTTPClient, loginWait, openBrowser = oldHTTP, oldWait, oldBrowser })
	loginHTTPClient = srv.Client
	loginWait = func(context.Context, time.Duration) error { return nil }
	openBrowser = func(string) error { t.Fatal("headless login opened browser"); return nil }
	path, store := browserLoginFixture(t, srv.URL)
	out, e := runBrowserLogin(t, context.Background(), path, "login", "--no-browser")
	if e == nil || !strings.Contains(out, "/cli/authorize?code=ABCD-EFGH") || strings.Contains(out, code) {
		t.Fatalf("first login error=%v output=%s", e, out)
	}
	if _, e = store.Load(srv.URL); !errors.Is(e, config.ErrNoCredentials) {
		t.Fatalf("unvalidated session saved: %v", e)
	}
	out, e = runBrowserLogin(t, context.Background(), path, "auth", "login", "--no-browser")
	if e == nil {
		t.Fatal("failed enrollment activated login")
	}
	if _, err := store.Load(srv.URL); !errors.Is(err, config.ErrNoCredentials) {
		t.Fatalf("profile activated before peer enrollment: %v", err)
	}
	if err := store.WithBrowserLogin(srv.URL, func(s *config.BrowserLoginState, _ func() error) error {
		if s.Credential == nil || s.Profile == nil || s.Profile.CLIClientSessionID != "cls_browser" {
			t.Error("approved login was not retained")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, e = runBrowserLogin(t, context.Background(), path, "auth", "login", "--no-browser")
	if e != nil {
		t.Fatal(e)
	}
	p, e := store.Load(srv.URL)
	if e != nil || p.Account.ID != "usr_browser" || p.CLIClientSessionID != "cls_browser" || authorizations != 1 || cancelled {
		t.Fatalf("recovery failed profile=%+v err=%v grants=%d canceled=%v", p, e, authorizations, cancelled)
	}
	assertBrowserPeerCertificate(t, store, srv.URL, p)
	if !strings.Contains(out, "Signed in as browser@example.test") {
		t.Fatal(out)
	}
	if _, e = runBrowserLogin(t, context.Background(), path, "login", "--json"); e == nil {
		t.Fatal("existing account unexpectedly replaced")
	}
}
func TestBrowserLoginCancelPreservesExistingAccount(t *testing.T) {
	code := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	cancelled := false
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/device/authorize":
			writeAPIData(t, w, map[string]any{"device_code": code, "user_code": "ABCD-EFGH", "verification_uri": srv.URL + "/cli/authorize", "verification_uri_complete": srv.URL + "/cli/authorize?code=ABCD-EFGH", "expires_in": 600, "interval": 1})
		case "/v1/auth/device/token":
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "authorization_pending"}})
		case "/v1/auth/device/cancel":
			cancelled = true
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	oldHTTP, oldWait := loginHTTPClient, loginWait
	t.Cleanup(func() { loginHTTPClient, loginWait = oldHTTP, oldWait })
	loginHTTPClient = srv.Client
	ctx, cancel := context.WithCancel(context.Background())
	loginWait = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	path, store := browserLoginFixture(t, srv.URL)
	if e := store.Save(config.Profile{Issuer: srv.URL, Account: config.Account{ID: "usr_old"}, CLIClientSessionID: "cls_old"}, config.Credential{AccessToken: "old-access", RefreshToken: "old-refresh"}); e != nil {
		t.Fatal(e)
	}
	_, e := runBrowserLogin(t, ctx, path, "auth", "login", "--change-account", "--no-browser")
	if !errors.Is(e, context.Canceled) || !cancelled {
		t.Fatalf("cancel err=%v called=%v", e, cancelled)
	}
	p, e := store.Load(srv.URL)
	if e != nil || p.Account.ID != "usr_old" {
		t.Fatal("old login lost")
	}
	if e = store.WithBrowserLogin(srv.URL, func(s *config.BrowserLoginState, _ func() error) error {
		if s.DeviceCode != "" {
			t.Error("canceled recovery retained")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
func TestBrowserLoginCommandContracts(t *testing.T) {
	r := newRootCommand()
	for _, path := range [][]string{{"login"}, {"auth", "login"}, {"switch"}, {"auth", "switch"}} {
		c, _, e := r.Find(path)
		if e != nil || c.Name() != path[len(path)-1] {
			t.Fatalf("missing %v", path)
		}
	}
	c, _, _ := r.Find([]string{"auth", "login"})
	if c.Flags().Lookup("token-file") != nil {
		t.Fatal("retired token login exposed")
	}
}

func acceptBrowserPeerEnrollment(t *testing.T, w http.ResponseWriter, r *http.Request, account, session string) {
	t.Helper()
	var input api.E2EEBootstrapInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		t.Error(err)
		w.WriteHeader(400)
		return
	}
	public, err := base64.RawURLEncoding.DecodeString(input.RootPublicKey)
	raw, rawErr := base64.RawURLEncoding.DecodeString(input.Certificate.Certificate)
	if err != nil || rawErr != nil {
		t.Error("invalid enrollment encoding")
		w.WriteHeader(400)
		return
	}
	if _, err = endpointidentity.Verify(raw, ed25519.PublicKey(public), endpointidentity.Expected{AccountID: account, Role: endpointidentity.RoleCLI, EndpointID: session, Generation: 1}, time.Now()); err != nil {
		t.Error(err)
		w.WriteHeader(400)
		return
	}
	if r.Header.Get("X-Paperboat-Fresh-Enrollment") != "1" || r.Header.Get("Idempotency-Key") == "" {
		t.Error("missing protected enrollment request")
	}
	digest := sha256.Sum256(public)
	fingerprint := hex.EncodeToString(digest[:])
	writeAPIData(t, w, api.E2EEBootstrapResult{KeyID: "aek_" + fingerprint, TrustedKeys: []api.E2EEKey{{KeyID: "aek_" + fingerprint, PublicKey: input.RootPublicKey, Fingerprint: fingerprint, Generation: 1}}, Certificate: input.Certificate})
}
func assertBrowserPeerCertificate(t *testing.T, store config.ProfileStore, issuer string, p config.Profile) {
	t.Helper()
	public, err := store.LoadPeerMachineSigningPublic(issuer, p.Account.ID)
	if err != nil {
		t.Fatalf("current peer verifier unavailable: %v", err)
	}
	raw, err := store.LoadPeerCertificate(issuer, p.CLIClientSessionID)
	if err != nil {
		t.Fatalf("current session certificate unavailable: %v", err)
	}
	if _, err = endpointidentity.Verify(raw.Raw, public, endpointidentity.Expected{AccountID: p.Account.ID, Role: endpointidentity.RoleCLI, EndpointID: p.CLIClientSessionID, Generation: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

type browserLoginFailingSecrets struct {
	config.SecretStore
	fail   string
	cancel context.CancelFunc
}

func (s *browserLoginFailingSecrets) Set(ref, value string) error {
	if s.fail != "" && strings.Contains(ref, s.fail) {
		return errors.New("injected credential storage failure")
	}
	err := s.SecretStore.Set(ref, value)
	if err == nil && s.cancel != nil && strings.Contains(ref, "endpoint-certificate") {
		s.cancel()
	}
	return err
}
func TestBrowserLoginPeerActivationPreservesPreviousCredentialsOnFailure(t *testing.T) {
	for _, phase := range []string{"certificate", "profile", "cancel"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/me" {
					writeAPIData(t, w, api.Me{ID: "usr_previous", Status: "active"})
					return
				}
				if r.URL.Path != "/v1/e2ee/bootstrap" {
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				acceptBrowserPeerEnrollment(t, w, r, "usr_previous", "cls_pending")
			}))
			defer srv.Close()
			oldHTTP := loginHTTPClient
			loginHTTPClient = srv.Client
			defer func() { loginHTTPClient = oldHTTP }()
			_, store := browserLoginFixture(t, srv.URL)
			oldProfile := config.Profile{Issuer: srv.URL, Account: config.Account{ID: "usr_previous"}, CLIClientSessionID: "cls_previous"}
			if err := store.Save(oldProfile, config.Credential{AccessToken: "previous-access", RefreshToken: "previous-refresh"}); err != nil {
				t.Fatal(err)
			}
			faulty := &browserLoginFailingSecrets{SecretStore: store.Secrets}
			if phase == "cancel" {
				faulty.cancel = cancel
			}
			if phase == "certificate" {
				faulty.fail = "endpoint-certificate"
			}
			if phase == "profile" {
				faulty.fail = "-access"
			}
			store.Secrets = faulty
			pending := config.BrowserLoginState{Issuer: srv.URL, PreviousSessionID: oldProfile.CLIClientSessionID, Profile: &config.Profile{Issuer: srv.URL, Account: oldProfile.Account, CLIClientSessionID: "cls_pending"}, Credential: &config.Credential{AccessToken: "pending-access", RefreshToken: "pending-refresh", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)}}
			complete := func(ctx context.Context) error {
				_, err := auth.CompleteLogin(ctx, auth.LoginCompletion{Store: store, Client: api.New(srv.URL, *pending.Credential, loginHTTPClient()), Issuer: srv.URL, Credential: *pending.Credential, SessionID: pending.Profile.CLIClientSessionID, ExpectedAccountID: pending.Profile.Account.ID, PreviousSessionID: &pending.PreviousSessionID})
				return err
			}
			err := complete(ctx)
			if err == nil {
				t.Fatal("failed operation activated profile")
			}
			if phase == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel classification: %v", err)
			}
			current, err := store.Load(srv.URL)
			if err != nil || current.CLIClientSessionID != oldProfile.CLIClientSessionID {
				t.Fatalf("previous profile lost: %v", err)
			}
			credential, err := store.CredentialFor(srv.URL)
			if err != nil || credential.AccessToken != "previous-access" {
				t.Fatal("previous credentials lost")
			}
			faulty.fail = ""
			faulty.cancel = nil
			loginHTTPClient = srv.Client
			if err := complete(context.Background()); err != nil {
				t.Fatal(err)
			}
			current, err = store.Load(srv.URL)
			if err != nil || current.CLIClientSessionID != "cls_pending" {
				t.Fatal("pending profile not activated on retry")
			}
			assertBrowserPeerCertificate(t, store, srv.URL, current)
		})
	}
}

func TestBrowserLoginEnrollsBeforeActivationAndRevocation(t *testing.T) {
	var store config.ProfileStore
	var issuer string
	revoked := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/me":
			writeAPIData(t, w, api.Me{ID: "usr_previous", Status: "active"})
		case "/v1/e2ee/bootstrap":
			p, err := store.Load(issuer)
			if err != nil || p.CLIClientSessionID != "cls_previous" {
				t.Error("profile activated before enrollment")
			}
			acceptBrowserPeerEnrollment(t, w, r, "usr_previous", "cls_pending")
		case "/v1/auth/token/revoke":
			p, err := store.Load(issuer)
			if err != nil || p.CLIClientSessionID != "cls_pending" {
				t.Error("previous session revoked before activation")
			}
			assertBrowserPeerCertificate(t, store, issuer, p)
			revoked = true
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	issuer = srv.URL
	oldHTTP := loginHTTPClient
	loginHTTPClient = srv.Client
	defer func() { loginHTTPClient = oldHTTP }()
	path, fixtureStore := browserLoginFixture(t, issuer)
	store = fixtureStore
	old := config.Profile{Issuer: issuer, Account: config.Account{ID: "usr_previous"}, CLIClientSessionID: "cls_previous"}
	if err := store.Save(old, config.Credential{AccessToken: "previous-access", RefreshToken: "previous-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithBrowserLogin(issuer, func(s *config.BrowserLoginState, save func() error) error {
		expires := time.Now().Add(time.Hour)
		*s = config.BrowserLoginState{Version: 1, Issuer: issuer, Interval: 1, ExpiresAt: expires, ApprovalURL: issuer + "/cli/authorize?code=ABCD-EFGH", DeviceCode: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)), PreviousSessionID: old.CLIClientSessionID, Profile: &config.Profile{Issuer: issuer, Account: old.Account, CLIClientSessionID: "cls_pending", AccessExpiresAt: expires}, Credential: &config.Credential{AccessToken: "pending-access", RefreshToken: "pending-refresh", TokenType: "Bearer", ExpiresAt: expires}}
		return save()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runBrowserLogin(t, context.Background(), path, "login", "--reauth", "--no-browser"); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("previous session revocation was not completed")
	}
	if err := store.WithBrowserLogin(issuer, func(s *config.BrowserLoginState, _ func() error) error {
		if s.DeviceCode != "" {
			t.Error("completed login recovery remains")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

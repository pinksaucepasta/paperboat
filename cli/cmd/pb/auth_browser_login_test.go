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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/spf13/cobra"
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
		case "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []any{}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 0, "next_offset": nil}})
		case "/v1/teams":
			writeAPIData(t, w, []any{})
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
	// An added, signed-in computer is told where it stands and is not signed in as someone else.
	oldMachine := loginLocalMachineID
	t.Cleanup(func() { loginLocalMachineID = oldMachine })
	loginLocalMachineID = func(*config.Config) (string, error) { return "machine_added", nil }
	again, e := runBrowserLogin(t, context.Background(), path, "login", "--json")
	if e != nil || !strings.Contains(again, "Already signed in as browser@example.test") || authorizations != 1 {
		t.Fatalf("existing account was not reported: %v %q grants=%d", e, again, authorizations)
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

func TestBrowserLoginCancellationFailureRetainsCauseAndReference(t *testing.T) {
	const reference = "support_40a66a76-2e81-4c21-b85d-1cd7860a108d"
	code := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	cancelAttempts := 0
	var cancelReferences []string
	authorizations := 0
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/device/authorize":
			authorizations++
			writeAPIData(t, w, map[string]any{"device_code": code, "user_code": "ABCD-EFGH", "verification_uri": srv.URL + "/cli/authorize", "verification_uri_complete": srv.URL + "/cli/authorize?code=ABCD-EFGH", "expires_in": 600, "interval": 1})
		case "/v1/auth/device/token":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "authorization_pending"}})
		case "/v1/auth/device/cancel":
			cancelAttempts++
			cancelReferences = append(cancelReferences, r.Header.Get(supportref.Header))
			if cancelAttempts == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "service_unavailable", "message": "private cancellation response"}})
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	oldHTTP, oldWait := loginHTTPClient, loginWait
	t.Cleanup(func() { loginHTTPClient, loginWait = oldHTTP, oldWait })
	loginHTTPClient = srv.Client
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loginWait = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	path, store := browserLoginFixture(t, srv.URL)
	if err := store.Save(config.Profile{Issuer: srv.URL, Account: config.Account{ID: "usr_old"}, CLIClientSessionID: "cls_old"}, config.Credential{AccessToken: "old-access", RefreshToken: "old-refresh"}); err != nil {
		t.Fatal(err)
	}
	ctx = supportref.WithContext(ctx, reference)
	_, err := runBrowserLogin(t, ctx, path, "auth", "login", "--change-account", "--no-browser")
	if !errors.Is(err, context.Canceled) || cancelAttempts != 1 {
		t.Fatalf("cancel request lost the user cancellation: err=%v attempts=%d", err, cancelAttempts)
	}
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("failed remote cancellation cause was discarded: %v", err)
	}
	if len(cancelReferences) != 1 || cancelReferences[0] != reference {
		t.Fatalf("cancel request lost its support reference: got=%q", cancelReferences)
	}
	if strings.Contains(err.Error(), "private cancellation response") || strings.Contains(err.Error(), code) {
		t.Fatalf("cancellation error exposed private response or device code: %v", err)
	}
	profile, err := store.Load(srv.URL)
	if err != nil || profile.Account.ID != "usr_old" {
		t.Fatalf("failed cancellation changed the active account: profile=%+v err=%v", profile, err)
	}
	if err := store.WithBrowserLogin(srv.URL, func(state *config.BrowserLoginState, _ func() error) error {
		if !state.Cancelled || state.DeviceCode != code {
			t.Errorf("failed cancellation did not retain a resumable grant: cancelled=%v present=%v", state.Cancelled, state.DeviceCode != "")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	resumeCtx, resumeCancel := context.WithCancel(context.Background())
	defer resumeCancel()
	resumeCtx = supportref.WithContext(resumeCtx, reference)
	loginWait = func(context.Context, time.Duration) error {
		resumeCancel()
		return context.Canceled
	}
	if _, err := runBrowserLogin(t, resumeCtx, path, "auth", "login", "--change-account", "--no-browser"); !errors.Is(err, context.Canceled) {
		t.Fatalf("resumed login did not preserve its later cancellation: %v", err)
	}
	if cancelAttempts != 3 || authorizations != 2 {
		t.Fatalf("retry did not cancel the retained grant before issuing a new one: cancellations=%d authorizations=%d", cancelAttempts, authorizations)
	}
	for _, got := range cancelReferences {
		if got != reference {
			t.Fatalf("resumed cancellation lost its support reference: %q", got)
		}
	}
	if err := store.WithBrowserLogin(srv.URL, func(state *config.BrowserLoginState, _ func() error) error {
		if state.DeviceCode != "" {
			t.Error("successful resume cleanup retained a canceled grant")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	profile, err = store.Load(srv.URL)
	if err != nil || profile.Account.ID != "usr_old" {
		t.Fatalf("cancellation retry changed the active account: profile=%+v err=%v", profile, err)
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
	onSet  func(ref string)
}

func (s *browserLoginFailingSecrets) Set(ref, value string) error {
	if s.fail != "" && strings.Contains(ref, s.fail) {
		return errors.New("injected credential storage failure")
	}
	err := s.SecretStore.Set(ref, value)
	if err == nil && s.onSet != nil {
		s.onSet(ref)
	}
	if err == nil && s.cancel != nil && strings.Contains(ref, "endpoint-certificate") {
		s.cancel()
	}
	return err
}
func TestBrowserLoginPeerActivationPreservesPreviousCredentialsOnFailure(t *testing.T) {
	for _, phase := range []string{"certificate", "profile", "cancel", "lock-release"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			const reference = "support_a33a480a-947c-4a20-bd74-6b8cbcc1ee88"
			if phase == "lock-release" {
				ctx = supportref.WithContext(ctx, reference)
			}
			ctx, cancel := context.WithCancel(ctx)
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
			var faults []errorreport.Fault
			injected := false
			restoreFaults := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
			defer restoreFaults()
			if phase == "lock-release" {
				faulty.onSet = func(ref string) {
					if !strings.HasSuffix(ref, "-access") {
						return
					}
					entries, readErr := os.ReadDir(filepath.Join(store.Path, "profiles"))
					if readErr != nil {
						t.Errorf("profile lock fixture unavailable: %v", readErr)
						return
					}
					profileFile := ""
					for _, entry := range entries {
						if !strings.HasSuffix(entry.Name(), ".json") {
							continue
						}
						b, err := os.ReadFile(filepath.Join(store.Path, "profiles", entry.Name()))
						var profile config.Profile
						if err == nil && json.Unmarshal(b, &profile) == nil && profile.Issuer == srv.URL {
							profileFile = entry.Name()
							break
						}
					}
					if profileFile == "" {
						t.Error("active profile lock fixture unavailable")
						return
					}
					ownerPath := filepath.Join(store.Path, "profiles", profileFile+".lock.d", "owner.json")
					if writeErr := os.WriteFile(ownerPath, []byte("{"), 0o600); writeErr != nil {
						t.Errorf("inject lock release failure: %v", writeErr)
						return
					}
					injected = true
				}
			}
			store.Secrets = faulty
			pending := config.BrowserLoginState{Issuer: srv.URL, PreviousSessionID: oldProfile.CLIClientSessionID, Profile: &config.Profile{Issuer: srv.URL, Account: oldProfile.Account, CLIClientSessionID: "cls_pending"}, Credential: &config.Credential{AccessToken: "pending-access", RefreshToken: "pending-refresh", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)}}
			complete := func(ctx context.Context) error {
				_, err := auth.CompleteLogin(ctx, auth.LoginCompletion{Store: store, Client: api.New(srv.URL, *pending.Credential, loginHTTPClient()), Issuer: srv.URL, Credential: *pending.Credential, SessionID: pending.Profile.CLIClientSessionID, ExpectedAccountID: pending.Profile.Account.ID, PreviousSessionID: &pending.PreviousSessionID})
				return err
			}
			err := complete(ctx)
			if phase == "lock-release" {
				if !injected {
					t.Fatal("profile lock release fault was not injected")
				}
				if err != nil {
					t.Fatalf("committed login was reported as failed: %v", err)
				}
				if len(faults) != 1 || faults[0].Operation != "login" || faults[0].Stage != "command" || faults[0].Code != "command_failed" || faults[0].Outcome != "failed" || faults[0].SupportReference != reference {
					t.Fatalf("committed cleanup failure was not observed once: %+v", faults)
				}
				if strings.Contains(strings.Join(faults[0].ErrorChain, " "), "pending-access") || strings.Contains(strings.Join(faults[0].ErrorChain, " "), "pending-refresh") {
					t.Fatal("committed cleanup diagnostic exposed credentials")
				}
				return
			}
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
	revokeAttempts := 0
	var revokeReference string
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
			revokeAttempts++
			revokeReference = r.Header.Get(supportref.Header)
			p, err := store.Load(issuer)
			if err != nil || p.CLIClientSessionID != "cls_pending" {
				t.Error("previous session revoked before activation")
			}
			assertBrowserPeerCertificate(t, store, issuer, p)
			if revokeAttempts == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "service_unavailable", "message": "private revocation response"}})
				return
			}
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
	const reference = "support_a33a480a-947c-4a20-bd74-6b8cbcc1ee88"
	ctx := supportref.WithContext(context.Background(), reference)
	var faults []errorreport.Fault
	restoreFaults := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
	defer restoreFaults()
	out, err := runBrowserLogin(t, ctx, path, "login", "--reauth", "--no-browser")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Previous session revocation is pending") || revokeAttempts != 1 || revoked {
		t.Fatalf("login did not preserve success while reporting pending revocation: output=%q attempts=%d revoked=%v", out, revokeAttempts, revoked)
	}
	if len(faults) != 1 || faults[0].Operation != "control_request" || faults[0].Stage != "control_request" || faults[0].Code != "control_request_failed" || faults[0].HTTPStatus != http.StatusServiceUnavailable || faults[0].SupportReference != reference || revokeReference != reference {
		t.Fatalf("nonterminal revocation outage was not observed once with its reference: faults=%+v request_reference=%q", faults, revokeReference)
	}
	if strings.Contains(strings.Join(faults[0].ErrorChain, " "), "private revocation response") {
		t.Fatalf("revocation response detail entered the safe fault: %+v", faults[0])
	}
	if records, err := store.PendingRevocations(issuer); err != nil || len(records) != 1 {
		t.Fatalf("failed revocation was not retained for recovery: records=%d err=%v", len(records), err)
	}
	if err := drainPendingRevocations(ctx, issuer, store); err != nil {
		t.Fatalf("pending revocation did not recover: %v", err)
	}
	if !revoked || revokeAttempts != 2 {
		t.Fatalf("recovery did not complete the pending revocation: attempts=%d revoked=%v", revokeAttempts, revoked)
	}
	if records, err := store.PendingRevocations(issuer); err != nil || len(records) != 0 {
		t.Fatalf("successful recovery retained pending revocation: records=%d err=%v", len(records), err)
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

func TestBrowserLoginPollDecodeFailureRetainsCauseAndPendingGrant(t *testing.T) {
	code := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{11}, 32))
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/device/authorize":
			writeAPIData(t, w, map[string]any{"device_code": code, "user_code": "ABCD-EFGH", "verification_uri": srv.URL + "/cli/authorize", "verification_uri_complete": srv.URL + "/cli/authorize?code=ABCD-EFGH", "expires_in": 600, "interval": 1})
		case "/v1/auth/device/token":
			_, _ = w.Write([]byte(`{"data":!"private-poll-content"}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	oldHTTP := loginHTTPClient
	loginHTTPClient = srv.Client
	t.Cleanup(func() { loginHTTPClient = oldHTTP })
	path, store := browserLoginFixture(t, srv.URL)
	var faults []errorreport.Fault
	restoreFaults := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
	defer restoreFaults()
	out, err := runBrowserLogin(t, context.Background(), path, "login", "--no-browser")
	if len(faults) != 1 {
		t.Fatalf("poll failure must be observed once across API and CLI owners: got %d", len(faults))
	}
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("poll failure lost original parser cause: %T", err)
	}
	if strings.Contains(out, "private-poll-content") || strings.Contains(err.Error(), "private-poll-content") {
		t.Fatal("poll content exposed")
	}
	if saveErr := store.WithBrowserLogin(srv.URL, func(state *config.BrowserLoginState, _ func() error) error {
		if state.DeviceCode != code || state.Cancelled || state.Credential != nil {
			t.Fatal("failed poll changed pending approval")
		}
		return nil
	}); saveErr != nil {
		t.Fatal(saveErr)
	}
}

type loginDeviceFixture struct {
	authorize  map[string]any
	enrollment map[string]any
}

func loginDeviceServer(t *testing.T, f *loginDeviceFixture) *httptest.Server {
	t.Helper()
	code := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/device/authorize":
			f.authorize = map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&f.authorize)
			writeAPIData(t, w, map[string]any{"device_code": code, "user_code": "ABCD-EFGH", "verification_uri": srv.URL + "/cli/authorize", "verification_uri_complete": srv.URL + "/cli/authorize?code=ABCD-EFGH", "expires_in": 600, "interval": 1})
		case "/v1/auth/device/token":
			token := map[string]any{"access_token": "test-access", "refresh_token": "test-refresh", "token_type": "Bearer", "expires_in": 3600, "cli_client_session_id": "cls_browser"}
			if f.enrollment != nil {
				token["enrollment"] = f.enrollment
			}
			writeAPIData(t, w, token)
		case "/v1/me":
			writeAPIData(t, w, map[string]string{"id": "usr_browser", "email": "browser@example.test", "status": "active"})
		case "/v1/e2ee/bootstrap":
			acceptBrowserPeerEnrollment(t, w, r, "usr_browser", "cls_browser")
		case "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []any{}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 0, "next_offset": nil}})
		case "/v1/teams":
			writeAPIData(t, w, []any{})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	oldHTTP, oldWait := loginHTTPClient, loginWait
	t.Cleanup(func() { loginHTTPClient, loginWait = oldHTTP, oldWait })
	loginHTTPClient = srv.Client
	loginWait = func(context.Context, time.Duration) error { return nil }
	return srv
}

// A device that is not a machine yet asks to join, pairs with the enrollment its approval
// carries, and reports where it was added.
func TestBrowserLoginAddsUnenrolledDevice(t *testing.T) {
	f := &loginDeviceFixture{enrollment: map[string]any{"bootstrap_token": "one-shot-token", "server_url": "https://api.example.test", "expires_at": time.Now().Add(10 * time.Minute), "workspace_id": "myteam"}}
	srv := loginDeviceServer(t, f)
	oldMachine, oldPair := loginLocalMachineID, pairLoginDevice
	t.Cleanup(func() { loginLocalMachineID, pairLoginDevice = oldMachine, oldPair })
	loginLocalMachineID = func(*config.Config) (string, error) { return "", nil }
	var pairedToken, pairedServer string
	pairLoginDevice = func(_ *command.Context, server string, e *config.LoginEnrollment, _ string) error {
		pairedToken, pairedServer = e.BootstrapToken, server
		return nil
	}
	path, _ := browserLoginFixture(t, srv.URL)
	out, err := runBrowserLogin(t, context.Background(), path, "login", "--no-browser", "--workspace", "myteam")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if f.authorize["enroll"] != true || f.authorize["workspace"] != "myteam" || f.authorize["enrolled_machine_id"] != nil {
		t.Fatalf("authorize body=%v", f.authorize)
	}
	if pairedToken != "one-shot-token" || pairedServer != srv.URL {
		t.Fatalf("paired with token=%q server=%q", pairedToken, pairedServer)
	}
	if !strings.Contains(out, "Code: ABCD-EFGH") || !strings.Contains(out, "to myteam.") {
		t.Fatalf("output=%s", out)
	}
}

// A machine that is already added only signs in again: it names itself and never pairs.
func TestBrowserLoginSignsInEnrolledDeviceOnly(t *testing.T) {
	f := &loginDeviceFixture{}
	srv := loginDeviceServer(t, f)
	oldMachine, oldPair := loginLocalMachineID, pairLoginDevice
	t.Cleanup(func() { loginLocalMachineID, pairLoginDevice = oldMachine, oldPair })
	loginLocalMachineID = func(*config.Config) (string, error) { return "machine_abc", nil }
	pairLoginDevice = func(*command.Context, string, *config.LoginEnrollment, string) error {
		t.Fatal("an enrolled machine paired again")
		return nil
	}
	path, _ := browserLoginFixture(t, srv.URL)
	out, err := runBrowserLogin(t, context.Background(), path, "login", "--no-browser", "--workspace", "myteam")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if f.authorize["enroll"] != nil || f.authorize["workspace"] != nil || f.authorize["enrolled_machine_id"] != "machine_abc" {
		t.Fatalf("authorize body=%v", f.authorize)
	}
	if strings.Contains(out, "Added") {
		t.Fatalf("output=%s", out)
	}
	again, err := runBrowserLogin(t, context.Background(), path, "login", "--no-browser")
	if err != nil || !strings.Contains(again, "Already signed in as browser@example.test on Personal.") || !strings.Contains(again, "pb login --fresh") {
		t.Fatalf("login on an added, signed-in machine: %v %s", err, again)
	}
}

// A team that approves new machines holds the computer: login says so instead of "Added".
func TestBrowserLoginReportsComputerWaitingForTeamAdmin(t *testing.T) {
	f := &loginDeviceFixture{enrollment: map[string]any{"bootstrap_token": "one-shot-token", "server_url": "https://api.example.test", "expires_at": time.Now().Add(10 * time.Minute), "workspace_id": "myteam", "admission": "pending"}}
	srv := loginDeviceServer(t, f)
	oldMachine, oldPair := loginLocalMachineID, pairLoginDevice
	t.Cleanup(func() { loginLocalMachineID, pairLoginDevice = oldMachine, oldPair })
	loginLocalMachineID = func(*config.Config) (string, error) { return "", nil }
	pairLoginDevice = func(*command.Context, string, *config.LoginEnrollment, string) error { return nil }
	path, _ := browserLoginFixture(t, srv.URL)
	out, err := runBrowserLogin(t, context.Background(), path, "login", "--no-browser", "--workspace", "myteam")
	if err != nil || !strings.Contains(out, "Waiting for an owner or admin of myteam to accept it.") || strings.Contains(out, "Added hp") {
		t.Fatalf("login: %v\n%s", err, out)
	}
}

// --fresh removes the computer's machine record and local setup, reinstalls, then adds it again.
func TestBrowserLoginFreshRemovesThenAdds(t *testing.T) {
	f := &loginDeviceFixture{enrollment: map[string]any{"bootstrap_token": "one-shot-token", "server_url": "https://api.example.test", "expires_at": time.Now().Add(10 * time.Minute), "workspace_id": "personal", "admission": "accepted"}}
	srv := loginDeviceServer(t, f)
	oldMachine, oldPair := loginLocalMachineID, pairLoginDevice
	oldRemove, oldCleanup, oldSource, oldInstall := removeOwnMachineRecordFunc, freshEnrollmentCleanup, runningInstallSource, installSuppliedExecutable
	t.Cleanup(func() {
		loginLocalMachineID, pairLoginDevice = oldMachine, oldPair
		removeOwnMachineRecordFunc, freshEnrollmentCleanup, runningInstallSource, installSuppliedExecutable = oldRemove, oldCleanup, oldSource, oldInstall
	})
	var steps []string
	removeOwnMachineRecordFunc = func(*cobra.Command) (string, error) { steps = append(steps, "remove"); return "machine_old", nil }
	freshEnrollmentCleanup = func(*cobra.Command) error { steps = append(steps, "cleanup"); return nil }
	runningInstallSource = func() (string, installsource.Source, error) { return "/tmp/pb", installsource.Source{}, nil }
	installSuppliedExecutable = func(context.Context, string, installsource.Source, string, string) (string, error) {
		steps = append(steps, "install")
		return "/tmp/pb", nil
	}
	loginLocalMachineID = func(*config.Config) (string, error) {
		if len(steps) == 0 {
			return "machine_old", nil
		}
		return "", nil
	}
	pairLoginDevice = func(*command.Context, string, *config.LoginEnrollment, string) error {
		steps = append(steps, "pair")
		return nil
	}
	path, _ := browserLoginFixture(t, srv.URL)
	out, err := runBrowserLogin(t, context.Background(), path, "login", "--no-browser", "--fresh")
	if err != nil {
		t.Fatalf("login --fresh: %v\n%s", err, out)
	}
	if strings.Join(steps, ",") != "remove,cleanup,install,pair" || f.authorize["enroll"] != true {
		t.Fatalf("steps=%v authorize=%v", steps, f.authorize)
	}
	if !strings.Contains(out, "Added") {
		t.Fatalf("output=%s", out)
	}
}

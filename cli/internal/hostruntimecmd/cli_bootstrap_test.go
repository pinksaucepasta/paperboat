package hostruntimecmd

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
)

func TestShouldInstallBootstrapCLI(t *testing.T) {
	session := &bootstrap.ClientSession{Schema: "paperboat.cli-session/v1"}
	if !shouldInstallBootstrapCLI(bootstrap.Material{ClientSession: session}) {
		t.Fatal("host enrollment must bootstrap the local CLI identity")
	}
	if !shouldInstallBootstrapCLI(bootstrap.Material{ClientSession: session}) {
		t.Fatal("client enrollment must bootstrap the local CLI identity")
	}
	if shouldInstallBootstrapCLI(bootstrap.Material{}) {
		t.Fatal("enrollment without a CLI session must not bootstrap one")
	}
}

func TestShouldInstallBootstrapRuntime(t *testing.T) {
	if !shouldInstallBootstrapRuntime(bootstrap.Material{UserMachineID: "machine", InstallationGeneration: 1}) {
		t.Fatal("enrolled machine must install managed runtime")
	}
	for _, material := range []bootstrap.Material{{}, {UserMachineID: "machine"}, {InstallationGeneration: 1}} {
		if shouldInstallBootstrapRuntime(material) {
			t.Fatal("unbound material installs runtime")
		}
	}
}

func TestBootstrapCLIHostEnrollmentInstallsClientProfileAndDaemon(t *testing.T) {
	store := testBootstrapProfileStore(t)
	issuer := "https://api.example.test"
	resume := bootstrap.NewResumeRecord(issuer, "public-key", "token", "Victus", "verifier-012345678901234567890123456789", time.Now().UTC().Add(time.Hour))
	material := bootstrap.Material{ClientSession: testBootstrapSession("cli_host")}
	installCalls := 0
	if err := completeBootstrapCLIResume(context.Background(), store.Path, issuer, material, &resume, func(_ context.Context, session *bootstrap.ClientSession, serverURL string) error {
		installCalls++
		if session == nil || session.SessionID != "cli_host" || serverURL != issuer {
			t.Fatalf("client bootstrap args = session=%#v server=%q", session, serverURL)
		}
		return nil
	}, bootstrap.SaveResume); err != nil {
		t.Fatal(err)
	}
	if installCalls != 1 || !resume.ClientInstalled {
		t.Fatalf("installCalls=%d clientInstalled=%t", installCalls, resume.ClientInstalled)
	}
	if _, err := os.Stat(bootstrap.ResumePath(store.Path)); err != nil {
		t.Fatalf("host resume checkpoint was not persisted: %v", err)
	}
}

func TestBootstrapCLICompletionUsesValidatedPeerEnrollment(t *testing.T) {
	for _, phase := range []string{"first", "same_account", "same_session", "cross_account", "inactive", "enrollment_retry"} {
		t.Run(phase, func(t *testing.T) {
			store := testBootstrapProfileStore(t)
			me := api.Me{ID: "account_1", Status: "active"}
			if phase == "cross_account" {
				me.ID = "account_other"
			}
			if phase == "inactive" {
				me.Status = "suspended"
			}
			failEnrollment := phase == "enrollment_retry"
			enrollmentCalls, daemonCalls := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var data any
				switch r.URL.Path {
				case "/v1/me":
					data = me
				case "/v1/e2ee/bootstrap":
					enrollmentCalls++
					if phase == "cross_account" || phase == "inactive" {
						t.Error("unauthorized account enrolled")
					}
					if phase != "first" {
						p, err := store.Load("http://" + r.Host)
						if err != nil || (p.CLIClientSessionID != "cli_old" && phase != "same_session") {
							t.Error("profile activated before enrollment")
						}
					}
					if failEnrollment {
						failEnrollment = false
						w.WriteHeader(503)
						return
					}
					var input api.E2EEBootstrapInput
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					public, err := base64.RawURLEncoding.DecodeString(input.RootPublicKey)
					if err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					raw, err := base64.RawURLEncoding.DecodeString(input.Certificate.Certificate)
					if err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if _, err := endpointidentity.Verify(raw, ed25519.PublicKey(public), endpointidentity.Expected{AccountID: "account_1", Role: endpointidentity.RoleCLI, EndpointID: "cli_new", Generation: 1}, time.Now()); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if r.Header.Get("Authorization") != "Bearer access-new" || r.Header.Get("X-Paperboat-Fresh-Enrollment") != "1" || r.Header.Get("Idempotency-Key") == "" {
						t.Error("invalid enrollment authority")
					}
					hash := sha256.Sum256(public)
					fp := hex.EncodeToString(hash[:])
					keyID := "aek_" + fp
					data = api.E2EEBootstrapResult{KeyID: keyID, TrustedKeys: []api.E2EEKey{{KeyID: keyID, PublicKey: input.RootPublicKey, Fingerprint: fp, Generation: 1}}, Certificate: input.Certificate}
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer srv.Close()
			issuer := srv.URL
			if phase != "first" {
				oldID := "cli_old"
				if phase == "same_session" {
					oldID = "cli_new"
				}
				if err := store.Save(config.Profile{Issuer: issuer, Account: config.Account{ID: "account_1"}, CLIClientSessionID: oldID}, testBootstrapCredential("old")); err != nil {
					t.Fatal(err)
				}
			}
			credential := testBootstrapCredential("new")
			install := func() error {
				return installBootstrapCLIWith(context.Background(), testBootstrapSession("cli_new"), issuer, store, credential, api.New(issuer, credential, srv.Client()), func(context.Context) error { daemonCalls++; return nil })
			}
			err := install()
			if phase == "cross_account" || phase == "inactive" {
				if err == nil || enrollmentCalls != 0 || daemonCalls != 0 {
					t.Fatalf("unauthorized completion: err=%v enrollment=%d daemon=%d", err, enrollmentCalls, daemonCalls)
				}
				p, e := store.Load(issuer)
				if e != nil || p.CLIClientSessionID != "cli_old" {
					t.Fatal("previous profile lost")
				}
				return
			}
			if phase == "enrollment_retry" {
				if err == nil || daemonCalls != 0 {
					t.Fatal("failed enrollment activated daemon")
				}
				p, e := store.Load(issuer)
				if e != nil || p.CLIClientSessionID != "cli_old" {
					t.Fatal("failed enrollment replaced previous profile")
				}
				pending, e := store.PendingRevocations(issuer)
				if e != nil || len(pending) != 0 {
					t.Fatal("failed enrollment queued old revocation")
				}
				err = install()
			}
			if err != nil {
				t.Fatal(err)
			}
			if daemonCalls != 1 {
				t.Fatal("daemon not installed after completion")
			}
			p, err := store.Load(issuer)
			if err != nil || p.CLIClientSessionID != "cli_new" {
				t.Fatal("current session not activated")
			}
			cert, err := store.LoadPeerCertificate(issuer, p.CLIClientSessionID)
			if err != nil {
				t.Fatal(err)
			}
			public, err := store.LoadPeerMachineSigningPublic(issuer, p.Account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := endpointidentity.Verify(cert.Raw, public, endpointidentity.Expected{AccountID: p.Account.ID, Role: endpointidentity.RoleCLI, EndpointID: p.CLIClientSessionID, Generation: 1}, time.Now()); err != nil {
				t.Fatal(err)
			}
			pending, err := store.PendingRevocations(issuer)
			expected := 1
			if phase == "first" || phase == "same_session" {
				expected = 0
			}
			if err != nil || len(pending) != expected {
				t.Fatalf("queued revocations=%d expected=%d err=%v", len(pending), expected, err)
			}
		})
	}
}

func testBootstrapProfileStore(t *testing.T) config.ProfileStore {
	t.Helper()
	dir := t.TempDir()
	return config.ProfileStore{Path: dir, Secrets: config.FileSecretStore{Dir: dir + "/secrets"}}
}

func testBootstrapSession(id string) *bootstrap.ClientSession {
	return &bootstrap.ClientSession{Schema: "paperboat.cli-session/v1", SessionID: id, AccessToken: "access-" + id, RefreshToken: "refresh-" + id, TokenType: "Bearer", ExpiresIn: 3600}
}

func testBootstrapCredential(label string) config.Credential {
	return config.Credential{AccessToken: "access-" + label, RefreshToken: "refresh-" + label, TokenType: "Bearer", ExpiresAt: time.Now().UTC().Add(time.Hour)}
}

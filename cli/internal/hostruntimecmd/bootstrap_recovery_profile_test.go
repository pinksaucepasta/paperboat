package hostruntimecmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

func TestBootstrapRecoveryRequiresCurrentEnrolledProfile(t *testing.T) {
	previousConfigHome, previousDownloads := xdg.ConfigHome, xdg.UserDirs.Download
	xdg.ConfigHome = t.TempDir()
	xdg.UserDirs.Download = filepath.Join(t.TempDir(), "Downloads")
	t.Cleanup(func() { xdg.ConfigHome, xdg.UserDirs.Download = previousConfigHome, previousDownloads })
	for _, scenario := range []string{"active", "refresh", "missing", "revoked", "foreign-account", "suspended", "changed-machine"} {
		t.Run(scenario, func(t *testing.T) {
			var meCalls, refreshCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/auth/token/refresh":
					refreshCalls.Add(1)
					var body struct {
						AttemptID string `json:"attempt_id"`
					}
					if r.Header.Get("Authorization") != "Bearer refresh-current" || json.NewDecoder(r.Body).Decode(&body) != nil || body.AttemptID == "" {
						t.Error("normal profile refresh lost its authority or replay binding")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TokenSet{AccessToken: "access-renewed", RefreshToken: "refresh-renewed", TokenType: "Bearer", ExpiresIn: 3600, CLIClientSessionID: "cli-current"}})
				case "/v1/me":
					meCalls.Add(1)
					token := "access-current"
					if scenario == "refresh" {
						token = "access-renewed"
					}
					if r.Header.Get("Authorization") != "Bearer "+token {
						t.Error("recovery replayed journal credentials")
					}
					if scenario == "revoked" {
						w.WriteHeader(http.StatusUnauthorized)
						_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "unauthorized", "message": "sign in"}})
						return
					}
					status := "active"
					if scenario == "suspended" {
						status = "suspended"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": api.Me{ID: "account_1", Status: status}})
				default:
					t.Error("recovery made an unexpected authorization request")
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			profiles, store, record, credential := bootstrapRecoveryProfileFixture(t, server.URL, scenario)
			err := validateBootstrapRecovery(context.Background(), store, record)
			allowed := scenario == "active" || scenario == "refresh"
			if (err == nil) != allowed {
				t.Fatalf("authorization accepted=%t wanted=%t error=%v", err == nil, allowed, err)
			}
			wantMe := int32(0)
			if allowed || scenario == "revoked" || scenario == "suspended" {
				wantMe = 1
			}
			wantRefresh := int32(0)
			if scenario == "refresh" {
				wantRefresh = 1
			}
			if meCalls.Load() != wantMe || refreshCalls.Load() != wantRefresh {
				t.Fatal("recovery bypassed current authority or refreshed outside its normal owner")
			}
			if allowed {
				before, err := store.Registration()
				if err != nil {
					t.Fatal(err)
				}
				if err := saveBootstrapRegistration(store, server.URL, *record.Material, "", 0); err != nil {
					t.Fatal(err)
				}
				after, err := store.Registration()
				if err != nil || after.InboxPath != before.InboxPath {
					t.Fatal("recovered bootstrap overwrote the existing Inbox choice")
				}
			}
			if scenario != "missing" {
				actual, err := profiles.CredentialFor(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				wanted := credential.RefreshToken
				if scenario == "refresh" {
					wanted = "refresh-renewed"
				}
				if actual.RefreshToken != wanted {
					t.Fatal("recovery replaced the current profile with installation credentials")
				}
			}
		})
	}
}

func bootstrapRecoveryProfileFixture(t *testing.T, serverURL, scenario string) (
	config.ProfileStore, *identity.Store, bootstrap.ResumeRecord, config.Credential,
) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerURL = serverURL
	cfg.Auth.AllowFileFallback = true
	cfg.Auth.ProfileDir = filepath.Join(t.TempDir(), "profiles")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfigPath, cfg.Path())
	profiles, err := config.ProfileStoreFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	credential := config.Credential{AccessToken: "access-current", RefreshToken: "refresh-current", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)}
	if scenario == "refresh" {
		credential.ExpiresAt = time.Now().Add(-time.Minute)
	}
	account := "account_1"
	if scenario == "foreign-account" {
		account = "account_other"
	}
	if scenario != "missing" {
		if err := profiles.Save(config.Profile{Issuer: serverURL, Account: config.Account{ID: account}, CLIClientSessionID: "cli-current", AccessExpiresAt: credential.ExpiresAt}, credential); err != nil {
			t.Fatal(err)
		}
	}
	store, err := identity.Open(identity.Config{StateRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	material := testClientBootstrapMaterial(serverURL, time.Now().Add(time.Hour))
	public := base64.RawURLEncoding.EncodeToString(store.Current().Public())
	registration := identity.Registration{ServerURL: serverURL, AccountID: "account_1", MachineID: material.UserMachineID, EnvironmentID: material.EnvironmentID, PublicKeyID: store.Current().ID, PublicIdentityKey: public, InboxPath: filepath.Join(t.TempDir(), "inbox"), InstallationGeneration: material.InstallationGeneration, UpdatedAt: time.Now()}
	if err := store.SaveRegistration(registration); err != nil {
		t.Fatal(err)
	}
	record := bootstrap.ResumeRecord{ServerURL: serverURL, PublicIdentityKey: public, Material: &material, ClientInstalled: true, RuntimeEnrolled: true}
	if scenario == "changed-machine" {
		material.UserMachineID = "machine_other"
	}
	return profiles, store, record, credential
}

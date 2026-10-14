package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type envIntegrityEmptyStore struct{}

func (envIntegrityEmptyStore) EnvironmentSecureStore() {}

func (envIntegrityEmptyStore) Get(string) (string, error) { return "", config.ErrSecretNotFound }
func (envIntegrityEmptyStore) Set(string, string) error   { return nil }
func (envIntegrityEmptyStore) Delete(string) error        { return nil }

func TestENVDecodedIntegrityKeepsHelpfulPresentationAndOneFaultOwner(t *testing.T) {
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	const canary = "PRIVATE-ENCRYPTED-INPUT"
	state := api.PasswordVaultState{Issuer: "https://control.example.test", AccountID: "account_1", Generation: 1, DocumentID: environmente2ee.DocumentID{}.String(), Envelope: "!" + canary}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": state})
	}))
	defer server.Close()
	ctx := supportref.WithContext(t.Context(), reference)
	client := api.New(server.URL, config.Credential{AccessToken: "fixture-token"}, server.Client())
	vault := environmentmanager.PasswordVault{WorkspaceID: "personal", Client: client, Issuer: state.Issuer, AccountID: state.AccountID, Store: config.ProfileStore{Path: filepath.Join(t.TempDir(), "profiles.json"), Secrets: envIntegrityEmptyStore{}}}
	err := vault.Initialize(ctx, []byte("unused-password"))
	var corrupt base64.CorruptInputError
	if !errors.As(err, &corrupt) || !errors.Is(err, environmentmanager.ErrIntegrity) || !onlyEnvironmentIntegrityFailure(err) {
		t.Fatal("actual API decoder cause lost its proven verification ownership")
	}
	presented := safeEnvironmentVariableCommandError(err)
	if presented.Error() != "encrypted ENV vault verification failed" || strings.Contains(presented.Error(), canary) || !errors.As(presented, &corrupt) || classifyCommandFailure(presented).kind != commandUnexpected {
		t.Fatal("verification presentation lost cause/privacy or suppressed unexpected capture")
	}
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
	defer restore()
	errorreport.Current().CaptureFailure(ctx, "paperboat-cli", "environment", "command", "command_failed", presented)
	if len(faults) != 1 || faults[0].SupportReference != reference {
		t.Fatal("final command did not own one correlated fault")
	}
	mixed := errors.Join(err, syscall.EIO)
	if onlyEnvironmentIntegrityFailure(mixed) || safeEnvironmentVariableCommandError(mixed).Error() != "environment variable update failed" || !errors.Is(mixed, syscall.EIO) {
		t.Fatal("decoder ownership concealed an independent operational fault")
	}
}

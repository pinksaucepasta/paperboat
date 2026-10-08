package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/config"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/spf13/cobra"
)

func TestAccountCredentialFailureKeepsPurposeAndIndependentCauses(t *testing.T) {
	cycle := &connectProofError{}
	cycle.cause = cycle
	var nilFailure *sessionauth.CredentialFailure
	for _, test := range []struct {
		name  string
		err   error
		kind  commandFailureKind
		login bool
	}{
		{"missing account token", &sessionauth.CredentialFailure{Cause: config.ErrSecretNotFound}, commandRejected, true},
		{"wrapped missing account token", &sessionauth.CredentialFailure{Cause: fmtCommandCause{cause: errors.Join(config.ErrSecretNotFound)}}, commandRejected, true},
		{"missing profile", &sessionauth.CredentialFailure{Cause: config.ErrNoCredentials}, commandRejected, true},
		{"missing account token and IO", &sessionauth.CredentialFailure{Cause: errors.Join(config.ErrSecretNotFound, syscall.EIO)}, commandUnexpected, false},
		{"cycle", &sessionauth.CredentialFailure{Cause: cycle}, commandUnexpected, false},
		{"missing cause", &sessionauth.CredentialFailure{}, commandUnexpected, false},
		{"nil owner", nilFailure, commandUnexpected, false},
		{"private pairing token", config.ErrSecretNotFound, commandRejected, false},
		{"expired account session", api.ErrUnauthenticated, commandRejected, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := classifyCommandFailure(test.err)
			message := userFacingError(test.err)
			if failure.kind != test.kind || strings.Contains(message, "`pb login`") != test.login {
				t.Fatal("account credential purpose concealed a fault or selected wrong recovery")
			}
			if test.kind == commandUnexpected {
				result := classifyCLIJSONFailure(test.err, failure)
				if result.Code != "operation_failed" || result.StateChanged != "unknown" || result.Retryable {
					t.Fatal("account wrapper asserted successful state or retry after a fault")
				}
			}
			if test.name == "missing account token and IO" && !errors.Is(test.err, syscall.EIO) {
				t.Fatal("original IO cause lost")
			}
			if test.name == "private pairing token" && !strings.Contains(message, "not paired for private transport") {
				t.Fatal("private custody guidance changed")
			}
		})
	}
}

func TestDoctorMissingAccountTokenUsesLoginAndRecovers(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"account_test"}}`))
	}))
	defer server.Close()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	writeTestProfile(t, directory, configPath, server.URL)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal("test configuration failed")
	}
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		t.Fatal("test credential store failed")
	}
	profile, err := store.Load(server.URL)
	if err != nil {
		t.Fatal("test profile load failed")
	}
	if err := store.Secrets.Delete(profile.AccessSecretRef); err != nil {
		t.Fatal("test credential deletion failed")
	}
	command := &cobra.Command{Use: "doctor"}
	command.Flags().String("config", configPath, "")
	command.Flags().String("server", "", "")
	check := probeDoctorAuthentication(t.Context(), command)
	if check.Status != doctorpkg.StatusFail || !strings.Contains(check.Recovery, "`pb login`") || strings.Contains(check.Recovery, "enrollment") || calls.Load() != 0 || check.Validate() != nil {
		t.Fatal("doctor missed account sign-in recovery or made an unauthenticated request")
	}
	if err := store.Secrets.Set(profile.AccessSecretRef, "token"); err != nil {
		t.Fatal("test credential replacement failed")
	}
	check = probeDoctorAuthentication(t.Context(), command)
	if check.Status != doctorpkg.StatusPass || calls.Load() != 1 || check.Validate() != nil {
		t.Fatal("doctor did not recover with one authenticated health request")
	}
}

func TestExecEnvironmentValidationDoesNotExposeValues(t *testing.T) {
	for _, values := range [][]string{
		{"INVALID-NAME=PRIVATE_ENV_VALUE"},
		{"TOKEN=PRIVATE_ENV_VALUE\x00"},
		{"TOKEN=PRIVATE_ENV_VALUE", "TOKEN=PRIVATE_ENV_VALUE"},
	} {
		result, err := parseExecEnvironment(values)
		if result != nil || err == nil || classifyCommandFailure(err).kind != commandUsage || strings.Contains(err.Error(), "PRIVATE_ENV_VALUE") || strings.Contains(userFacingError(err), "PRIVATE_ENV_VALUE") {
			t.Fatal("environment argument validation exposed a value or became an operational fault")
		}
	}
	result, err := parseExecEnvironment([]string{"TOKEN=PRIVATE_ENV_VALUE=with-equals"})
	if err != nil || result["TOKEN"] != "PRIVATE_ENV_VALUE=with-equals" {
		t.Fatal("valid environment assignment changed")
	}
}

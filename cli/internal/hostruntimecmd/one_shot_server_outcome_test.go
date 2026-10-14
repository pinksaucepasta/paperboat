package hostruntimecmd

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
)

// serverBootstrapOutcome returns the error shape produced by a real control
// plane response, which wraps the outcome with its HTTP diagnostic cause.
func serverBootstrapOutcome(t *testing.T, status int, code string) error {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"server outcome"}}`))
	}))
	t.Cleanup(server.Close)
	config := bootstrap.Config{ServerURL: server.URL, Alias: "laptop", WorkspaceRoot: t.TempDir(), Verifier: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), PublicIdentityKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), HTTP: server.Client()}
	_, err := bootstrap.RecoverMaterial(context.Background(), config, false)
	if err == nil {
		t.Fatal("server outcome was accepted")
	}
	return err
}

func TestOneShotFreshPairingCreatesPairingAfterServerUnknownVerifier(t *testing.T) {
	now := time.Now().UTC()
	root := t.TempDir()
	server := "https://api.example.test"
	publicKey := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	token := "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP"
	unknown := serverBootstrapOutcome(t, http.StatusGone, "user_machine_installation_unavailable")
	resume, resumeErr := bootstrap.LoadResume(root, server, publicKey, token, "Laptop", now)
	createCalls, waitCalls := 0, 0
	operations := testOneShotResumeOperations(now)
	operations.WaitForMaterial = func(context.Context, bootstrap.Config, time.Time, time.Duration) (bootstrap.Material, error) {
		waitCalls++
		if waitCalls == 1 {
			return bootstrap.Material{}, unknown
		}
		return testClientBootstrapMaterial(server, now.Add(time.Hour)), nil
	}
	operations.CreatePairing = func(context.Context, bootstrap.Config) (bootstrap.Pairing, error) {
		createCalls++
		return bootstrap.Pairing{ID: "pairing", UserCode: "CODE", ExpiresAt: now.Add(time.Hour)}, nil
	}
	input := testOneShotResumeInput(root, server, publicKey, token, resume, resumeErr)
	material, _, err := resumeOneShotEnrollment(context.Background(), input, operations)
	if err != nil {
		t.Fatalf("fresh pairing stopped at the first server poll: %v", err)
	}
	if material.UserMachineID != "mch_client" || createCalls != 1 {
		t.Fatalf("material=%#v create=%d", material, createCalls)
	}
	if !bootstrap.OnlyOutcome(serverBootstrapOutcome(t, http.StatusGone, "user_machine_pairing_expired"), bootstrap.ErrPairingExpired, bootstrap.ErrPairingDenied) {
		t.Fatal("expired server outcome was not classified")
	}
	if bootstrap.OnlyOutcome(serverBootstrapOutcome(t, http.StatusServiceUnavailable, "temporarily_unavailable"), bootstrap.ErrInstallationUnavailable) {
		t.Fatal("unavailable service was classified as an unknown verifier")
	}
}

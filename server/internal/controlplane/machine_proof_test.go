package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

func TestVerifyMachineControlRequestRejectsHelperCredentialWithoutFallback(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	signer, err := mint.NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := signer.SignCredential(mint.CredentialInput{
		Issuer: "https://api.example.test", Audience: "paperboat-control", Subject: "helper_1",
		JTI: "helper_jti_1", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
		CredentialClass: "helper_identity", Scopes: []string{"helper:connect", "helper:renew"},
		EnvironmentID: "environment_1", HelperID: "helper_1", MachineID: "machine_1",
		KeyThumbprint: "sha256:helper",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &EnrollmentService{signer: signer, issuer: "https://api.example.test", clock: func() time.Time { return now }}
	if _, err := service.VerifyMachineControlRequest(context.Background(), helper, []byte("proof"), "POST", "/v1/edge/private-access/grants", []byte(`{}`)); !errors.Is(err, ErrHelperProof) {
		t.Fatalf("helper credential error=%v, want ErrHelperProof", err)
	}
}

func TestMachineProofVerificationPreservesDatabaseFailures(t *testing.T) {
	store := openControlPlaneTestDB(t)
	now := time.Now().UTC()
	signer, err := mint.NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	machineCredential, err := signer.SignCredential(mint.CredentialInput{
		Issuer: "https://api.example.test", Audience: "paperboat-control", Subject: "machine_1",
		JTI: "machine_jti_1", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
		CredentialClass: "machine_control", Scopes: []string{"machine:connect", "machine:renew"},
		EnvironmentID: "environment_1", MachineID: "machine_1", UserID: "user_1",
		KeyThumbprint: "sha256:test", InstallationGeneration: 1, SessionGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	helperCredential, err := signer.SignCredential(mint.CredentialInput{
		Issuer: "https://api.example.test", Audience: "paperboat-control", Subject: "helper_1",
		JTI: "helper_jti_1", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
		CredentialClass: "helper_identity", Scopes: []string{"helper:connect", "helper:renew"},
		EnvironmentID: "environment_1", HelperID: "helper_1", MachineID: "machine_1", KeyThumbprint: "sha256:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service := NewEnrollmentService(store, signer, nil, "https://api.example.test", "")
	service.clock = func() time.Time { return now }
	for name, verify := range map[string]func() error{
		"machine-control verifier": func() error {
			_, err := service.VerifyMachineControlRequest(context.Background(), machineCredential, []byte("proof"), "POST", GrantPathForMachineProofTest, []byte(`{}`))
			return err
		},
		"machine verifier fallback": func() error {
			_, err := service.VerifyMachineRequest(context.Background(), machineCredential, []byte("proof"), "POST", GrantPathForMachineProofTest, []byte(`{}`))
			return err
		},
		"helper verifier": func() error {
			_, err := service.VerifyHelperRequest(context.Background(), helperCredential, []byte("proof"), "POST", GrantPathForMachineProofTest, []byte(`{}`))
			return err
		},
		"runtime observation verifier": func() error {
			return service.VerifyRuntimeObservation(context.Background(), machineCredential, []byte("proof"), []byte(`{}`), "environment_1", "machine_1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verify(); err == nil || errors.Is(err, ErrHelperProof) {
				t.Fatalf("verification err=%v; want the underlying database failure, not an invalid-proof result", err)
			}
		})
	}
}

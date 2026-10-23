package admission

import (
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"testing"
	"time"
)

func TestIssuerUsesConfiguredAdmissionLifetimeAndExactBindings(t *testing.T) {
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := New(signer, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	issued, err := issuer.Issue(mint.CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-enrollment", Subject: "environment_test", JTI: "enrollment_token", IssuedAt: now, CredentialClass: "helper_enrollment", Scopes: []string{"helper:enroll"}, EnvironmentID: "environment_test", EnrollmentID: "enrollment_test"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := signer.VerifyCredential(issued.Token, "https://api.example.test", "helper_enrollment", now)
	if err != nil || claims.EnvironmentID != "environment_test" || claims.EnrollmentID != "enrollment_test" || claims.ExpiresAt != now.Add(2*time.Second).Unix() || !issued.ExpiresAt.Equal(now.Add(2*time.Second)) {
		t.Fatal("configured lifetime or binding lost")
	}
	if _, err := signer.VerifyCredential(issued.Token, "https://api.example.test", "helper_enrollment", now.Add(3*time.Second)); err == nil {
		t.Fatal("expired credential admitted a new connection")
	}
}

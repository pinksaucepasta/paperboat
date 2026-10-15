package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

type credentialLifetimeClock struct{ now time.Time }

func (c *credentialLifetimeClock) Now() time.Time { return c.now }

type credentialLifetimeKeys struct{ public ed25519.PublicKey }

func (k credentialLifetimeKeys) Lookup(context.Context, string) (ed25519.PublicKey, bool, error) {
	return k.public, true, nil
}
func (k credentialLifetimeKeys) Refresh(context.Context) error { return nil }

func TestAdmittedCredentialAuthorizerSurvivesExpiryAndEnforcesRevocationAndBinding(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	clock := &credentialLifetimeClock{now: time.Now().Truncate(time.Second)}
	claims := auth.Claims{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "user", UserID: "user", JTI: "credential_1", IssuedAt: clock.now.Unix(), ExpiresAt: clock.now.Add(time.Second).Unix(), CredentialClass: "exec_operation", Scope: []string{"exec:operate"}, EnvironmentID: "env", MachineID: "machine", OperationID: "operation_exec", CLIClientSessionID: "cli", AssignmentID: "access"}
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-credential+jwt", "kid": "key"})
	payload, _ := json.Marshal(claims)
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	token := unsigned + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(unsigned)))
	revocations := auth.NewRevocationCache()
	verifier := auth.Verifier{Keys: credentialLifetimeKeys{public}, Clock: clock, Revocations: revocations}
	resolver := resolverFunc(func(f protocol.Frame) (auth.Policy, error) {
		return auth.Policy{Issuer: claims.Issuer, Audience: claims.Audience, CredentialClass: claims.CredentialClass, Scopes: claims.Scope, EnvironmentID: claims.EnvironmentID, MachineID: claims.MachineID, OperationID: f.OperationID}, nil
	})
	admitted := &CredentialAuthorizer{Token: token, Verifier: verifier, Resolver: resolver, Revocations: revocations}
	defer admitted.CloseAuthorization()
	frame := protocol.Frame{Capability: "exec.v1", OperationID: claims.OperationID}
	if _, err := admitted.Authorize(t.Context(), frame); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(2 * time.Second)
	if _, err := admitted.Authorize(t.Context(), frame); err != nil {
		t.Fatal("expiry ended admitted operation", err)
	}
	fresh := &CredentialAuthorizer{Token: token, Verifier: verifier, Resolver: resolver, Revocations: revocations}
	defer fresh.CloseAuthorization()
	var rejected *auth.Error
	if _, err := fresh.Authorize(t.Context(), frame); !errors.As(err, &rejected) || rejected.Code != auth.Expired {
		t.Fatal("expired credential admitted new connection", err)
	}
	changed := frame
	changed.OperationID = "other_operation"
	if _, err := admitted.Authorize(t.Context(), changed); err == nil {
		t.Fatal("admitted authority crossed operation binding")
	}
	if err := revocations.Replace([]string{claims.JTI}); err != nil {
		t.Fatal(err)
	}
	if _, err := admitted.Authorize(t.Context(), frame); err == nil {
		t.Fatal("revoked admitted credential remained usable")
	}
}

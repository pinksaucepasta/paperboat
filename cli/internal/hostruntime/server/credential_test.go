package server

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

type verifierFunc func(context.Context, string, auth.Policy) (auth.Claims, error)

func (f verifierFunc) Verify(ctx context.Context, token string, policy auth.Policy) (auth.Claims, error) {
	return f(ctx, token, policy)
}

type resolverFunc func(protocol.Frame) (auth.Policy, error)

func (f resolverFunc) Policy(frame protocol.Frame) (auth.Policy, error) { return f(frame) }

func TestCredentialAuthorizerReturnsStableBoundIdentity(t *testing.T) {
	claims := auth.Claims{Issuer: "https://api.test", Subject: "usr_1", JTI: "jti_1", IssuedAt: 1, ExpiresAt: 100, Scope: []string{"terminal:operate"}, CredentialClass: "terminal_operation", EnvironmentID: "env_1", UserID: "usr_1", CLIClientSessionID: "cli_1", SessionID: "ses_1", AssignmentID: "asn_1"}
	authorizer := CredentialAuthorizer{
		Token: "signed-token",
		Resolver: resolverFunc(func(frame protocol.Frame) (auth.Policy, error) {
			if frame.Capability != "terminal.v1" {
				return auth.Policy{}, ErrCredentialPolicy
			}
			return auth.Policy{Audience: "paperboat-machine"}, nil
		}),
		Verifier: verifierFunc(func(_ context.Context, token string, policy auth.Policy) (auth.Claims, error) {
			if token != "signed-token" || policy.Audience != "paperboat-machine" {
				return auth.Claims{}, errors.New("bad verification input")
			}
			return claims, nil
		}),
	}
	first, err := authorizer.Authorize(context.Background(), protocol.Frame{Capability: "terminal.v1"})
	if err != nil {
		t.Fatal(err)
	}
	claims.JTI, claims.IssuedAt, claims.ExpiresAt = "jti_2", 50, 150
	claims.AssignmentID = "asn_2"
	// A renewed signed credential is admitted on a new authenticated connection.
	renewed := CredentialAuthorizer{Token: authorizer.Token, Verifier: authorizer.Verifier, Resolver: authorizer.Resolver}
	second, err := renewed.Authorize(context.Background(), protocol.Frame{Capability: "terminal.v1"})
	if err != nil {
		t.Fatal(err)
	}
	if first.JournalBinding == "" || first.JournalBinding != second.JournalBinding {
		t.Fatalf("renewed binding first=%q second=%q", first.JournalBinding, second.JournalBinding)
	}
	if first.ResourceID != "asn_1" || second.ResourceID != "asn_2" {
		t.Fatalf("credential-instance resource changed: first=%q second=%q", first.ResourceID, second.ResourceID)
	}
	claims.SessionID = "ses_2"
	different := CredentialAuthorizer{Token: authorizer.Token, Verifier: authorizer.Verifier, Resolver: authorizer.Resolver}
	third, err := different.Authorize(context.Background(), protocol.Frame{Capability: "terminal.v1"})
	if err != nil {
		t.Fatal(err)
	}
	if third.JournalBinding == first.JournalBinding {
		t.Fatal("resource change did not change journal binding")
	}
}

func TestCredentialAuthorizerFailsClosedWithoutPolicy(t *testing.T) {
	authorizer := CredentialAuthorizer{Token: "token", Verifier: verifierFunc(func(context.Context, string, auth.Policy) (auth.Claims, error) { return auth.Claims{}, nil })}
	if _, err := authorizer.Authorize(context.Background(), protocol.Frame{}); !errors.Is(err, ErrCredentialPolicy) {
		t.Fatalf("err=%v", err)
	}
}

func TestCredentialAuthorizerDerivesSharedRoleAndRequiresBindings(t *testing.T) {
	claims := auth.Claims{CredentialClass: "terminal_operation", Scope: []string{"terminal:view"}, SessionID: "ses_shared", AccountID: "acc_1", CLIClientSessionID: "cli_1", ExpectedGeneration: 3, ExpiresAt: 100}
	authorizer := CredentialAuthorizer{Token: "signed", Resolver: resolverFunc(func(protocol.Frame) (auth.Policy, error) { return auth.Policy{}, nil }), Verifier: verifierFunc(func(context.Context, string, auth.Policy) (auth.Claims, error) { return claims, nil })}
	got, err := authorizer.Authorize(context.Background(), protocol.Frame{Capability: "terminal.v1"})
	if err != nil || got.TerminalRole != TerminalRoleViewer || got.SessionID != "ses_shared" || got.AccountID != "acc_1" || got.TerminalGeneration != 3 {
		t.Fatalf("authorization=%#v err=%v", got, err)
	}
	claims.SessionID = ""
	invalid := CredentialAuthorizer{Token: authorizer.Token, Verifier: authorizer.Verifier, Resolver: authorizer.Resolver}
	if _, err := invalid.Authorize(context.Background(), protocol.Frame{Capability: "terminal.v1"}); !errors.Is(err, ErrCredentialPolicy) {
		t.Fatalf("empty session err=%v", err)
	}
}

func TestCredentialAuthorizerUsesCodexSessionAsResource(t *testing.T) {
	claims := auth.Claims{Issuer: "https://api.test", Subject: "usr_1", JTI: "jti_1", IssuedAt: 1, ExpiresAt: 100, Scope: []string{"codex:connect"}, CredentialClass: "codex_connect", EnvironmentID: "env_1", MachineID: "machine_1", UserID: "usr_1", CLIClientSessionID: "cli_1", SessionID: "cdx_1"}
	authorizer := CredentialAuthorizer{Token: "signed-token", Resolver: resolverFunc(func(protocol.Frame) (auth.Policy, error) { return auth.Policy{}, nil }), Verifier: verifierFunc(func(context.Context, string, auth.Policy) (auth.Claims, error) { return claims, nil })}
	got, err := authorizer.Authorize(context.Background(), protocol.Frame{Capability: "codex.connect.v1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ResourceID != "cdx_1" {
		t.Fatalf("resource id=%q", got.ResourceID)
	}
}

func TestStableClaimsBindingKeepsRenewalButSeparatesResources(t *testing.T) {
	for _, class := range []string{"terminal_operation", "exec_operation", "ssh_operation", "file_transfer", "config_compare"} {
		t.Run(class, func(t *testing.T) {
			claims := auth.Claims{Issuer: "https://api.test", Subject: "usr_1", JTI: "jti_1", IssuedAt: 1, ExpiresAt: 100, Scope: []string{"operate"}, CredentialClass: class, MachineID: "machine_1", UserID: "usr_1", CLIClientSessionID: "cli_1", OperationID: "operation_1", AssignmentID: "access_session_1"}
			first, err := stableClaimsBinding(claims)
			if err != nil {
				t.Fatal(err)
			}
			claims.JTI, claims.IssuedAt, claims.ExpiresAt = "jti_2", 50, 150
			claims.AssignmentID = "access_session_2"
			renewed, err := stableClaimsBinding(claims)
			if err != nil {
				t.Fatal(err)
			}
			if class == "config_compare" {
				if renewed == first {
					t.Fatal("distinct config assignments shared a binding")
				}
				return
			}
			if renewed != first {
				t.Fatal("renewed access session changed durable binding")
			}
			for name, mutate := range map[string]func(*auth.Claims){
				"operation": func(c *auth.Claims) { c.OperationID = "operation_2" },
				"principal": func(c *auth.Claims) { c.UserID = "usr_2" },
				"scope":     func(c *auth.Claims) { c.Scope = []string{"view"} },
				"machine":   func(c *auth.Claims) { c.MachineID = "machine_2" },
			} {
				different := claims
				mutate(&different)
				binding, err := stableClaimsBinding(different)
				if err != nil || binding == first {
					t.Fatalf("%s crossed binding boundary: %v", name, err)
				}
			}
		})
	}
}

func TestStableClaimsBindingSeparatesWorkspaceAndLaunchActor(t *testing.T) {
	claims := auth.Claims{Issuer: "https://api.test", Subject: "usr_1", CredentialClass: "terminal_operation", WorkspaceID: "personal", ActorAccountID: "usr_1"}
	first, err := stableClaimsBinding(claims)
	if err != nil {
		t.Fatal(err)
	}
	claims.WorkspaceID = "team-one"
	team, err := stableClaimsBinding(claims)
	if err != nil || team == first {
		t.Fatal("workspace reused operation binding")
	}
	claims.ActorAccountID = "usr_2"
	actor, err := stableClaimsBinding(claims)
	if err != nil || actor == team {
		t.Fatal("actor reused operation binding")
	}
}

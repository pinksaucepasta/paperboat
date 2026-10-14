package usermachines

import (
	"context"
	"testing"
)

func TestCredentialKindRevocationUsesAuthoritativeKind(t *testing.T) {
	issuer := &recordingIssuer{}
	service := &Service{credentials: issuer}
	for _, id := range []string{"token_2eed839c-08e1-41c4-870b-2f96a5631f37", "token_123e4567-e89b-42d3-a456-426614174000"} {
		if err := service.revokeCredentialSessions(context.Background(), machineAccessSession{CredentialKind: machineCredentialExternal, TerminalSessionID: id}, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if len(issuer.revocations) != 2 {
		t.Fatalf("external issuer revocations=%d", len(issuer.revocations))
	}
	if err := service.revokeCredentialSessions(context.Background(), machineAccessSession{CredentialKind: machineCredentialSigned, TerminalSessionID: "token_123e4567-e89b-42d3-a456-426614174000"}, "test"); err != nil {
		t.Fatal(err)
	}
	if len(issuer.revocations) != 2 {
		t.Fatal("signed credential sent to external issuer")
	}
	for _, kind := range []machineCredentialKind{"", "unknown"} {
		if err := service.revokeCredentialSessions(context.Background(), machineAccessSession{CredentialKind: kind, TerminalSessionID: "token_53cbb3f7-ec2a-4ae8-9cd2-06d7c39cedd2"}, "test"); err == nil {
			t.Fatalf("kind %q admitted without authority", kind)
		}
	}
}

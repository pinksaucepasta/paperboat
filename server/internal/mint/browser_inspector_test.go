package mint

import (
	"strings"
	"testing"
	"time"
)

func TestBrowserInspectorCredentialHasNoTerminalBindings(t *testing.T) {
	provider, err := NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	base := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "usr_1", JTI: "jti_inspector_1", IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "browser_inspector_operation", Scopes: []string{"inspector:use"}, EnvironmentID: "env_1", AccountID: "usr_1", MachineID: "machine_1", UserID: "usr_1", BrowserAttachmentID: "attachment_1", BrowserPublicKeySHA256: strings.Repeat("A", 43)}
	token, err := provider.SignCredential(base)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, base.Issuer, base.CredentialClass, now)
	if err != nil || claims.MachineID != "machine_1" || claims.UserID != "usr_1" || claims.BrowserAttachmentID != "attachment_1" || claims.SessionID != "" || claims.Scopes[0] != "inspector:use" {
		t.Fatalf("inspector claims = %#v, %v", claims, err)
	}
	if _, err := provider.VerifyCredential(token, base.Issuer, "browser_terminal_operation", now); err == nil {
		t.Fatal("inspector credential accepted as a browser terminal credential")
	}
	for name, mutate := range map[string]func(*CredentialInput){
		"terminal session":  func(in *CredentialInput) { in.SessionID = "term_1" },
		"generation":        func(in *CredentialInput) { in.ExpectedGeneration = 1 },
		"policy generation": func(in *CredentialInput) { in.PolicyGeneration = 1 },
		"terminal scope":    func(in *CredentialInput) { in.Scopes = []string{"terminal:operate"} },
		"no attachment":     func(in *CredentialInput) { in.BrowserAttachmentID = "" },
		"no browser key":    func(in *CredentialInput) { in.BrowserPublicKeySHA256 = "" },
		"other account":     func(in *CredentialInput) { in.AccountID = "usr_2" },
		"no machine":        func(in *CredentialInput) { in.MachineID = "" },
	} {
		invalid := base
		mutate(&invalid)
		if _, err := provider.SignCredential(invalid); err == nil {
			t.Fatalf("accepted inspector credential with %s", name)
		}
	}
}

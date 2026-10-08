package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func TestConfigCompareCredentialHasExactClassScopeAndGeneration(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	verifier := auth.Verifier{Keys: staticKeys{keys: map[string]ed25519.PublicKey{"key": public}}, Clock: staticClock{now}}
	config := CredentialAuthConfig{Issuer: "https://control.test", EnvironmentID: "env", MachineID: "machine", HelperID: "helper", InstallationGeneration: 3, Verifier: verifier}
	native, err := NewCredentialAuthorizer(config)
	if err != nil {
		t.Fatal(err)
	}
	browser, err := NewBrowserConfigCompareCredentialAuthorizer(config)
	if err != nil {
		t.Fatal(err)
	}
	claims := auth.Claims{Issuer: config.Issuer, Audience: "paperboat-machine", Subject: "user", UserID: "user", AccountID: "account", JTI: "grant", IssuedAt: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(), CredentialClass: "config_compare", Scope: []string{"config:compare"}, EnvironmentID: "env", MachineID: "machine", InstallationGeneration: 3, SourceMachineID: "source", CLIClientSessionID: "cli", AssignmentID: "assignment", AssignmentVersion: 2, ConfigPath: "settings.txt", ConflictRevision: "conflict", ExpectedRemoteRevision: "remote"}
	token := signStaticCredential(t, private, "key", claims)
	authorization, err := native(token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authorization.Authorize(context.Background(), protocol.Frame{Capability: "config.compare.v1"}); err != nil {
		t.Fatal(err)
	}
	for _, capability := range []string{"terminal.v1", "exec.v1", "config.apply.v1", "file-transfer.v1"} {
		if _, err := authorization.Authorize(context.Background(), protocol.Frame{Capability: capability}); err == nil {
			t.Fatalf("compare credential authorized %s", capability)
		}
	}
	claims.InstallationGeneration = 2
	stale, _ := native(signStaticCredential(t, private, "key", claims))
	if _, err := stale.Authorize(context.Background(), protocol.Frame{Capability: "config.compare.v1"}); err == nil {
		t.Fatal("stale installation accepted")
	}
	claims.InstallationGeneration = 3
	claims.CredentialClass = "browser_config_compare"
	claims.SourceMachineID = ""
	claims.CLIClientSessionID = ""
	claims.BrowserAttachmentID = "read_1"
	claims.BrowserPublicKeySHA256 = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	token = signStaticCredential(t, private, "key", claims)
	browserAuth, _ := browser(token)
	if _, err := browserAuth.Authorize(context.Background(), protocol.Frame{Capability: "config.compare.v1"}); err != nil {
		t.Fatal(err)
	}
	plain, _ := native(token)
	if _, err := plain.Authorize(context.Background(), protocol.Frame{Capability: "config.compare.v1"}); err == nil {
		t.Fatal("browser credential accepted by plain transport")
	}
	if _, err := browserAuth.Authorize(context.Background(), protocol.Frame{Capability: "terminal.v1"}); err == nil {
		t.Fatal("browser compare opened a terminal")
	}
}

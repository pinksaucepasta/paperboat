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
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/shared/recording"
)

// A replay ticket opens exactly one recording on this machine, over the browser E2E channel (with
// its browser key) or natively (with its CLI session), and nothing else.
func TestStaticAuthorizerBindsReplayTicketsToOneRecording(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	verifier := auth.Verifier{Keys: staticKeys{keys: map[string]ed25519.PublicKey{"key-1": public}}, Clock: staticClock{now}, ClockSkew: time.Minute}
	config := CredentialAuthConfig{Issuer: "https://control.test", EnvironmentID: "env_test", MachineID: "machine_test", HelperID: "hlp_test", Verifier: verifier}
	browserFactory, err := NewBrowserTerminalCredentialAuthorizer(config)
	if err != nil {
		t.Fatal(err)
	}
	nativeFactory, err := NewCredentialAuthorizer(config)
	if err != nil {
		t.Fatal(err)
	}
	const id = "rec_00000000-0000-4000-8000-000000000001"
	native := auth.Claims{Issuer: "https://control.test", Audience: "paperboat-machine", Subject: "usr_test", JTI: "jti_replay", IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(4 * time.Minute).Unix(), Scope: []string{recording.Scope}, CredentialClass: recording.CredentialClass, EnvironmentID: "env_test", AccountID: "usr_test", UserID: "usr_test", MachineID: "machine_test", ResourceKind: recording.ResourceKind, ResourceID: id, CLIClientSessionID: "cli_test"}
	browser := native
	browser.CLIClientSessionID, browser.BrowserAttachmentID, browser.BrowserPublicKeySHA256 = "", "att_replay", base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	replay := protocol.Frame{Type: "request", Capability: recording.Capability}

	authorizer, err := browserFactory(signStaticCredential(t, private, "key-1", browser))
	if err != nil {
		t.Fatal(err)
	}
	if pin, channel, err := authorizer.(server.BrowserTerminalCredentialKey).BrowserTerminalPublicKeySHA256(context.Background()); err != nil || channel != server.BrowserChannelRecording || pin != browser.BrowserPublicKeySHA256 {
		t.Fatalf("browser replay pin=%q channel=%q err=%v", pin, channel, err)
	}
	got, err := authorizer.Authorize(context.Background(), replay)
	if err != nil || !got.BrowserRecording || got.BrowserTerminal || got.ResourceID != id || got.ClientID != "att_replay" || got.AccountID != "usr_test" {
		t.Fatalf("browser replay authorization=%#v err=%v", got, err)
	}
	for _, frame := range []protocol.Frame{{Type: "request", Capability: "terminal.v1"}, {Type: "request", Capability: "file-transfer.v1"}, {Type: "ack"}, {Type: "request", Capability: "inspector.v1"}} {
		if _, err := authorizer.Authorize(context.Background(), frame); err == nil {
			t.Fatalf("replay ticket authorized %+v", frame)
		}
	}
	plain, err := nativeFactory(signStaticCredential(t, private, "key-1", native))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := plain.Authorize(context.Background(), replay); err != nil || got.BrowserRecording || got.ResourceID != id || got.ClientID != "cli_test" {
		t.Fatalf("native replay authorization=%#v err=%v", got, err)
	}
	for name, mutate := range map[string]func(*auth.Claims){
		"other_kind":       func(c *auth.Claims) { c.ResourceKind = "preview" },
		"bad_recording":    func(c *auth.Claims) { c.ResourceID = "../x" },
		"no_cli_session":   func(c *auth.Claims) { c.CLIClientSessionID = "" },
		"half_browser":     func(c *auth.Claims) { c.BrowserAttachmentID = "att_x" },
		"terminal_session": func(c *auth.Claims) { c.SessionID = "ses_x" },
		"account_mismatch": func(c *auth.Claims) { c.AccountID = "usr_other" },
		"terminal_scope":   func(c *auth.Claims) { c.Scope = []string{"terminal:operate"} },
		"long_lived":       func(c *auth.Claims) { c.ExpiresAt = now.Add(time.Hour).Unix() },
		"other_machine":    func(c *auth.Claims) { c.MachineID = "machine_other" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := native
			mutate(&invalid)
			candidate, err := nativeFactory(signStaticCredential(t, private, "key-1", invalid))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := candidate.Authorize(context.Background(), replay); err == nil {
				t.Fatal("invalid replay ticket authorized")
			}
		})
	}
}

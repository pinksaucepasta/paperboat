package tunnel

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/resolver"
)

// The login user an `ssh` stream names reaches the host in its authorization header; other consumers never carry one.
func TestSSHAuthorizationHeaderCarriesTheLoginUserOnlyForSSH(t *testing.T) {
	now := time.Now()
	target := &resolver.TerminalTarget{LoginUser: "macuser", Auth: resolver.AuthTarget{Token: "credential", ExpiresAt: now.Add(time.Minute).UTC().Format(time.RFC3339)}}
	application := peerApplication{operationID: "operation_1"}
	header, err := application.authorizationHeader(target, "ssh", "fallback", "stream_1", now)
	if err != nil || header.LoginUser != "macuser" {
		t.Fatalf("ssh header: %+v %v", header, err)
	}
	// A Windows target registers DOMAIN\user (os/user on Windows): the header cannot carry it, and with it the
	// stream to every Windows machine failed before it opened.
	target.LoginUser = `WIN-TARGET\Tester`
	header, err = application.authorizationHeader(target, "ssh", "fallback", "stream_1", now)
	if _, marshalErr := header.MarshalBinary(); err != nil || marshalErr != nil || header.LoginUser != "" {
		t.Fatalf("ssh header for a qualified Windows user: %+v %v %v", header, err, marshalErr)
	}
	target.LoginUser = "macuser"
	header, err = application.authorizationHeader(target, "terminal", "fallback", "stream_1", now)
	if err != nil || header.LoginUser != "" {
		t.Fatalf("terminal header: %+v %v", header, err)
	}
}

// `pb --workspace TEAM exec` on a credential-free (standing-rule) stream names the team in the header so the host asks the
// server for that team's ENV layers; an online stream (its credential names the workspace) never carries one.
func TestOfflineAuthorizationHeaderCarriesTheRequestedWorkspace(t *testing.T) {
	now := time.Now()
	expires := now.Add(time.Minute).UTC().Format(time.RFC3339)
	application := peerApplication{operationID: "operation_1"}
	offline := &resolver.TerminalTarget{Workspace: "team-a", Auth: resolver.AuthTarget{Offline: true, ExpiresAt: expires}}
	header, err := application.authorizationHeader(offline, "exec", "fallback", "stream_1", now)
	if _, marshalErr := header.MarshalBinary(); err != nil || marshalErr != nil || header.Workspace != "team-a" {
		t.Fatalf("offline header: %+v %v %v", header, err, marshalErr)
	}
	offline.Workspace = ""
	if header, err = application.authorizationHeader(offline, "exec", "fallback", "stream_1", now); err != nil || header.Workspace != "" {
		t.Fatalf("offline header without a workspace: %+v %v", header, err)
	}
	online := &resolver.TerminalTarget{Workspace: "team-a", Auth: resolver.AuthTarget{Token: "credential", ExpiresAt: expires}}
	if header, err = application.authorizationHeader(online, "exec", "fallback", "stream_1", now); err != nil || header.Workspace != "" {
		t.Fatalf("online header: %+v %v", header, err)
	}
}

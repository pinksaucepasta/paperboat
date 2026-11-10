package tunnel

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// The login user an `ssh` stream names reaches the host in its authorization header; other consumers never carry one.
func TestSSHAuthorizationHeaderCarriesTheLoginUserOnlyForSSH(t *testing.T) {
	now := time.Now()
	target := &resolver.TerminalTarget{LoginUser: "pbmac2", Auth: resolver.AuthTarget{Token: "credential", ExpiresAt: now.Add(time.Minute).UTC().Format(time.RFC3339)}}
	application := peerApplication{operationID: "operation_1"}
	header, err := application.authorizationHeader(target, "ssh", "fallback", "stream_1", now)
	if err != nil || header.LoginUser != "pbmac2" {
		t.Fatalf("ssh header: %+v %v", header, err)
	}
	// A Windows target registers DOMAIN\user (os/user on Windows): the header cannot carry it, and with it the
	// stream to every Windows machine failed before it opened.
	target.LoginUser = `VICTUS\Pujan`
	header, err = application.authorizationHeader(target, "ssh", "fallback", "stream_1", now)
	if _, marshalErr := header.MarshalBinary(); err != nil || marshalErr != nil || header.LoginUser != "" {
		t.Fatalf("ssh header for a qualified Windows user: %+v %v %v", header, err, marshalErr)
	}
	target.LoginUser = "pbmac2"
	header, err = application.authorizationHeader(target, "terminal", "fallback", "stream_1", now)
	if err != nil || header.LoginUser != "" {
		t.Fatalf("terminal header: %+v %v", header, err)
	}
}

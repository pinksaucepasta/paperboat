package tunnel

import (
	"errors"
	"fmt"
	"net/url"
	"testing"

	clientapi "github.com/pinksaucepasta/paperboat/shared/client/api"

	"github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

type failingAuth struct{ err error }

func (a failingAuth) Credential() (config.Credential, error) { return config.Credential{}, a.err }

// An offline dial survives an access token whose refresh cannot reach Paperboat (F5-08 M: a client whose 10-minute
// token expired during the outage could not start `pb ssh`); an online dial and any other failure still stop.
func TestDialCredentialOfflineSurvivesUnreachableRefresh(t *testing.T) {
	unreachable := &auth.CredentialFailure{Cause: fmt.Errorf("refresh Paperboat session: %w", &clientapi.CallFailure{Message: "paperboat-server request failed", Err: &url.Error{Op: "Post", URL: "https://example.test/refresh", Err: errors.New("connect: invalid argument")}})}
	for _, test := range []struct {
		name    string
		err     error
		offline bool
		fails   bool
	}{
		{"offline, refresh unreachable", unreachable, true, false},
		{"online, refresh unreachable", unreachable, false, true},
		{"offline, signed out", &auth.SignedOutError{Cause: &auth.CredentialFailure{Cause: config.ErrNoCredentials}}, true, true},
	} {
		_, err := dialCredential(failingAuth{test.err}, test.offline)
		if (err != nil) != test.fails {
			t.Errorf("%s: err = %v, want failure %v", test.name, err, test.fails)
		}
	}
}

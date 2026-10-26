package main

import (
	"context"
	"fmt"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

// F5-09: each SSH refusal reaches the user in pb's own words, not the generic "could not finish the command".
func TestSSHRefusalsAreShownInPbsWords(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{&managedssh.RefusedError{Reason: managedssh.RefusedNoAccess}, "do not have SSH access"},
		{&managedssh.RefusedError{Reason: managedssh.RefusedNotSetUp}, "SSH is not set up"},
		{fmt.Errorf("proxy: %w", managedssh.ErrSSHServerClosed), "SSH server closed the connection"},
		{explainSSHDial("box", errors.Join(localapi.ErrTransportUnavailable, errors.New("dial unix"))), "Paperboat service is not running"},
		{explainSSHDial("box", fmt.Errorf("open peer stream: %w", context.DeadlineExceeded)), "could not reach box"},
		{explainSSHDial("box", &localapi.RemoteError{StatusCode: http.StatusServiceUnavailable, Code: "peer_transport_unavailable"}), "could not reach box"},
	} {
		message := commandFailureMessage(test.err, classifyCommandFailure(test.err))
		if !strings.Contains(message, test.want) || strings.Contains(message, "could not finish the command") {
			t.Fatalf("%v shown as %q, want %q", test.err, message, test.want)
		}
	}
}

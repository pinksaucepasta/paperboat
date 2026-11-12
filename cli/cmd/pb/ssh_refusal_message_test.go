package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
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
		{errors.Join(managedssh.ErrSSHLoopbackOwner, errors.New("tcp table")), "another program connected"},
		{fmt.Errorf("listener_accept: %w", managedssh.ErrSSHLoopbackAccept), "did not connect to it in time"},
		{managedssh.ErrSSHLoopbackProcessExited, "exited before connecting"},
		{managedssh.ErrSSHLoopbackShutdown, "did not exit after the session ended"},
		{managedssh.NativeLaunchError{Err: errors.New("ssh.exe not found")}, "native SSH or transfer tool could not start"},
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

// A Mac whose Remote Login excludes the managed user: pb says so, instead of "Connection closed".
func TestSSHRefusalForADeniedRemoteLoginUserNamesTheUserAndTheSetting(t *testing.T) {
	err := &managedssh.RefusedError{Reason: managedssh.RefusedLoginNotAllowed + ":pbmac2"}
	message := commandFailureMessage(err, classifyCommandFailure(err))
	for _, want := range []string{"pbmac2 isn't allowed to log in over SSH", "System Settings", "Remote Login"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q lacks %q", message, want)
		}
	}
}

func TestNativeLaunchMessageNamesAMissingTool(t *testing.T) {
	missing := managedssh.NativeLaunchError{Err: &exec.Error{Name: "rsync", Err: exec.ErrNotFound}}
	got := nativeLaunchMessage(missing)
	for _, want := range []string{"rsync was not found on this computer", "pb scp", "pb ssh doctor"} {
		if !strings.Contains(got, want) {
			t.Fatalf("message %q lacks %q", got, want)
		}
	}
	other := nativeLaunchMessage(managedssh.NativeLaunchError{Err: errors.New("permission denied")})
	if !strings.Contains(other, "native SSH or transfer tool could not start") {
		t.Fatalf("other launch failures keep the generic message: %q", other)
	}
}

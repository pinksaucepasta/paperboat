package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// The local service's failure codes say which side is not ready; nothing falls back to a message
// that hides the step that failed.
func TestLocalAPIFailureNamesWhichSideIsNotReady(t *testing.T) {
	for _, test := range []struct{ code, want string }{
		{"peer_transport_unavailable", "could not reach the target machine"},
		{"peer_stream_unavailable", "did not open the connection"},
		{"inventory_unavailable", "local Paperboat service is still starting"},
		{"snapshot_unavailable", "local Paperboat service is still starting"},
		{"file_transfer_unavailable", "(step: file_transfer_unavailable)"},
		{"vault_custody_unavailable", "(step: vault_custody_unavailable)"},
	} {
		err := &localapi.RemoteError{StatusCode: http.StatusServiceUnavailable, Code: test.code, Message: "private detail"}
		got := commandFailureMessage(err, classifyCommandFailure(err))
		if !strings.Contains(got, test.want) || strings.Contains(got, "private detail") {
			t.Errorf("%s: message = %q, want %q and no daemon message", test.code, got, test.want)
		}
	}
}

func TestLocalAPIFailureNeverEchoesACodeThatIsNotAnAuthoredConstant(t *testing.T) {
	err := &localapi.RemoteError{StatusCode: http.StatusServiceUnavailable, Code: "Bad Code: /home/user/secret", Message: "x"}
	got := commandFailureMessage(err, classifyCommandFailure(err))
	if strings.Contains(got, "secret") || strings.Contains(got, "step:") || !strings.Contains(got, "pb status") {
		t.Fatalf("message = %q", got)
	}
}

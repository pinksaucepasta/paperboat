package main

import (
	"net/url"
	"strings"
	"testing"
)

type previewTestTimeout struct{}

func (previewTestTimeout) Error() string   { return "Client.Timeout exceeded while awaiting headers" }
func (previewTestTimeout) Timeout() bool   { return true }
func (previewTestTimeout) Temporary() bool { return true }

// team_two_account.py on pbmac 2026-11-05: the local daemon did not answer the owner-session lease of
// `pb preview --background` within its two 5 s attempts while the Mac was overloaded, and the CLI said
// only "Paperboat could not finish the command". The message names the local service and what to do.
func TestPreviewOwnerSessionFailureNamesTheLocalService(t *testing.T) {
	err := previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable,
		&url.Error{Op: "Post", URL: "http://127.0.0.1:1/v1/preview/owner-sessions", Err: previewTestTimeout{}})
	message := commandFailureMessage(err, classifyCommandFailure(err))
	if !strings.Contains(message, "local Paperboat service") || !strings.Contains(message, "pb doctor") || strings.Contains(message, "could not finish the command") {
		t.Fatalf("message = %q", message)
	}
}

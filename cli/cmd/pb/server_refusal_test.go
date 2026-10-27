package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func serverRefusal(status int, code, message string) *api.APIError {
	return &api.APIError{Status: status, Code: code, Message: "The server rejected the request.", ServerMessage: message}
}

// A 4xx says what the server said, with its code, in the human message and in --json, for any status.
func TestServerRefusalIsShownInItsOwnWordsWithItsCode(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity} {
		err := serverRefusal(status, "preview_team_mode_conflict", "This preview already belongs to another team")
		message := commandFailureMessage(err, classifyCommandFailure(err))
		if !strings.Contains(message, "This preview already belongs to another team") || !strings.Contains(message, "preview_team_mode_conflict") {
			t.Errorf("%d: message = %q", status, message)
		}
		if encoded := classifyCLIJSONError(err); encoded.Code != "preview_team_mode_conflict" {
			t.Errorf("%d: json code = %q", status, encoded.Code)
		}
	}
}

// A session refusal (401) and a server error (5xx) keep their own handling.
func TestServerRefusalKeepsSessionAndServerErrorsAsTheyWere(t *testing.T) {
	if encoded := classifyCLIJSONError(serverRefusal(http.StatusUnauthorized, "unauthenticated", "x")); encoded.Category != "authentication" {
		t.Errorf("401 json = %#v", encoded)
	}
	err := serverRefusal(http.StatusServiceUnavailable, "upstream_down", "database down")
	if message := commandFailureMessage(err, classifyCommandFailure(err)); strings.Contains(message, "database down") {
		t.Errorf("a 5xx must not echo the server's words: %q", message)
	}
}

// A code the CLI already words itself keeps its own message; the server's words are only for codes it has no text for.
func TestServerRefusalDoesNotReplaceTheCLIsOwnMessage(t *testing.T) {
	err := &api.APIError{Status: http.StatusConflict, Code: "team_subscription_active", Message: api.PublicMessage("team_subscription_active"), ServerMessage: "Cancel the team subscription."}
	if message := apiErrorFallback(err); !strings.Contains(message, "Billing") || strings.Contains(message, "(team_subscription_active)") {
		t.Errorf("message = %q", message)
	}
	if encoded := classifyCLIJSONError(err); encoded.Code != "team_subscription_active" {
		t.Errorf("json code = %q", encoded.Code)
	}
}

func TestServerRefusalNeverEchoesACodeThatIsNotAConstant(t *testing.T) {
	err := serverRefusal(http.StatusBadRequest, "Bad Code /home/x", "Nope")
	message := commandFailureMessage(err, classifyCommandFailure(err))
	if strings.Contains(message, "/home/x") || !strings.Contains(message, "Nope") {
		t.Errorf("message = %q", message)
	}
}

func TestBoundedServerMessage(t *testing.T) {
	if got := api.BoundedServerMessage("  a\x1b[31mb\nc  "); got != "a[31mbc" {
		t.Errorf("got %q", got)
	}
	if got := api.BoundedServerMessage(strings.Repeat("x", 1000)); len(got) != 256 {
		t.Errorf("len = %d", len(got))
	}
}

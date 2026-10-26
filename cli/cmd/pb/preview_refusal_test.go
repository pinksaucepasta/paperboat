package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// pb preview reports the server's answer when it is the only cause (team_limit on a full team workspace), keeps a
// sibling failure unexpected, and still never echoes an unknown server code or message.
func TestPreviewFailureReportsALoneServerRefusal(t *testing.T) {
	full := &api.APIError{Status: http.StatusConflict, Code: "team_limit", Message: api.PublicMessage("team_limit")}
	err := previewFailure("preview could not be started", full)
	if got := classifyCLIJSONError(err); got.Code != "team_limit" || !strings.Contains(got.Message, "Team capacity reached") {
		t.Fatalf("lone refusal: %#v", got)
	}
	mixed := previewFailure("preview setup failed and carrier cleanup was incomplete", full, errors.New("carrier close failed"))
	if got := classifyCLIJSONError(mixed); got.Code != "operation_failed" || got.StateChanged != "unknown" {
		t.Fatalf("a sibling failure must stay unexpected: %#v", got)
	}
	unknown := previewFailure("preview could not be started", &api.APIError{Status: http.StatusBadRequest, Code: "private_api_token_value", Message: "private response body"})
	if got := classifyCLIJSONError(unknown); got.Code != "api_error" || strings.Contains(got.Message, "private") {
		t.Fatalf("unknown server code echoed: %#v", got)
	}
}

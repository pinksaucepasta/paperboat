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

// An errors.Join whose cleanup succeeded (the shape pb access tunnel, pb access machine, pb inbox and pb exec
// return) has one real cause, so a lone server refusal is reported; a second cause or a typed wrapper with its own
// meaning is not peeled.
func TestLoneServerRefusalInAJoinIsReported(t *testing.T) {
	full := &api.APIError{Status: http.StatusConflict, Code: "team_limit", Message: api.PublicMessage("team_limit")}
	var cleanup error // a cleanup that succeeded
	if got := classifyCLIJSONError(errors.Join(full, cleanup)); got.Code != "team_limit" {
		t.Fatalf("lone cause in a join: %#v", got)
	}
	for name, err := range map[string]error{
		"two causes":                errors.Join(full, errors.New("close failed")),
		"typed nil beside it":       errors.Join(full, (*commandPrivateError)(nil)),
		"delivered receipt wrapper": deliveredReceiptFailure{causes: []error{full}},
		"terminal sharing wrapper":  terminalSharingFailure{causes: []error{full}},
	} {
		if got := classifyCLIJSONError(err); got.Code == "team_limit" {
			t.Errorf("%s: the refusal was presented as the whole story: %#v", name, got)
		}
	}
}

package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
)

// F6-05: sending to an offline computer is refused before anything is sent.
func TestSendRefusalsReportNothingChanged(t *testing.T) {
	cases := map[string]struct {
		err        error
		code, text string
	}{
		"offline": {&api.APIError{Status: http.StatusConflict, Code: "machine_offline", Message: "The destination machine is offline."}, "machine_offline", "offline"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result := classifyCLIJSONError(tc.err)
			if result.Code != tc.code || result.StateChanged != false || !strings.Contains(strings.ToLower(result.Message), tc.text) {
				t.Fatalf("code=%q state_changed=%v message=%q", result.Code, result.StateChanged, result.Message)
			}
		})
	}
}

// F6-05: a send over a transfer limit says which limit and by how much, and that nothing was sent.
func TestSendOverTheTransferLimitsIsACodedRefusal(t *testing.T) {
	paths := make([]string, 12)
	for i := range paths {
		paths[i] = fmt.Sprintf("/tmp/paperboat-limit-%d", i)
	}
	_, tooMany := filetransfer.Prepare(paths, filetransfer.Limits{})
	cases := map[string]struct {
		err  error
		text string
	}{
		"too many files": {tooMany, "That is 12 files and one send takes at most 10"},
		"large file":     {&filetransfer.LimitError{Scope: filetransfer.LimitFileBytes, Name: "big.bin", Value: 51 << 20, Limit: 50 << 20}, "big.bin is 51 MiB and a single file can be at most 50 MiB"},
		"batch bytes":    {fmt.Errorf("prepare: %w", &filetransfer.LimitError{Scope: filetransfer.LimitBatchBytes, Value: 600 << 20, Limit: 500 << 20}), "total 600 MiB and one send takes at most 500 MiB"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var limit *filetransfer.LimitError
			if !errors.As(tc.err, &limit) {
				t.Fatalf("not a limit error: %v", tc.err)
			}
			failure := classifyCommandFailure(tc.err)
			message := commandFailureMessage(tc.err, failure)
			result := classifyCLIJSONError(tc.err)
			if failure.kind != commandRejected || !strings.Contains(message, tc.text) || !strings.Contains(message, "Nothing was sent") && !strings.Contains(message, "nothing was sent") ||
				result.Code != "file_transfer_limit_exceeded" || result.StateChanged != false || result.Message != message {
				t.Fatalf("kind=%v message=%q json=%+v", failure.kind, message, result)
			}
		})
	}
}

// F12-06: a team change while the team is suspended names the suspension in text and JSON.
func TestTeamSuspendedRefusalNamesTheSuspension(t *testing.T) {
	err := &api.APIError{Status: http.StatusConflict, Code: "team_suspended"}
	failure := classifyCommandFailure(err)
	message := commandFailureMessage(err, failure)
	result := classifyCLIJSONError(err)
	if !strings.Contains(message, "This team is suspended") || strings.Contains(message, "Refresh it") ||
		result.Code != "team_suspended" || result.Category != "conflict" || result.StateChanged != false || !strings.Contains(result.Message, "suspended") {
		t.Fatalf("message=%q json=%+v", message, result)
	}
}

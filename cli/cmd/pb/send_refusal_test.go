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

// F6-05: a receiver whose disk is full refuses at once and the sender names it, in text and JSON.
func TestSendToAFullDiskNamesTheComputerAndStopsRetrying(t *testing.T) {
	remote := &filetransfer.Error{Code: "insufficient_storage", StatusCode: http.StatusInsufficientStorage, NeededBytes: 20 << 20, FreeBytes: 4 << 20}
	err := destinationStorageFull("hp", fmt.Errorf("send batch: %w", remote))
	failure := classifyCommandFailure(err)
	message := commandFailureMessage(err, failure)
	for _, want := range []string{"hp has no room for this in its Inbox", "needs 20 MiB, 4 MiB free", "pb inbox set", "Nothing was delivered"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q lacks %q", message, want)
		}
	}
	result := classifyCLIJSONError(err)
	if failure.kind != commandRejected || result.Code != "destination_storage_full" || result.StateChanged != false || result.Retryable {
		t.Fatalf("kind=%v json=%+v", failure.kind, result)
	}
	// It ran out of room while receiving (the size was not known up front).
	late := destinationStorageFull("hp", &filetransfer.Error{Code: "insufficient_storage", StatusCode: http.StatusInsufficientStorage})
	if m := commandFailureMessage(late, classifyCommandFailure(late)); !strings.Contains(m, "ran out of room in its Inbox while receiving") {
		t.Fatalf("late message %q", m)
	}
	other := &filetransfer.Error{Code: "storage_unavailable", StatusCode: http.StatusServiceUnavailable}
	if destinationStorageFull("hp", other) != error(other) {
		t.Fatal("another failure was converted")
	}
}

// F6-05: a team policy stricter than the CLI default is reported as the limit that was exceeded.
func TestServerTransferLimitNamesTheExceededLimit(t *testing.T) {
	refusal := &api.APIError{Status: http.StatusRequestEntityTooLarge, Code: "file_transfer_limit_exceeded", Details: map[string]any{"max_file_bytes": float64(10 << 20), "max_batch_files": float64(10), "max_batch_bytes": float64(500 << 20)}}
	sources := []filetransfer.Source{{Basename: "movie.mp4", Size: 30 << 20, Reader: strings.NewReader("x")}}
	err := serverTransferLimit(refusal, sources)
	if err == nil || !strings.Contains(commandFailureMessage(err, classifyCommandFailure(err)), "movie.mp4 is 30 MiB and a single file can be at most 10 MiB") {
		t.Fatalf("limit = %v", err)
	}
	if serverTransferLimit(&api.APIError{Code: "something_else"}, sources) != nil || serverTransferLimit(refusal, nil) != nil {
		t.Fatal("converted something that is not an exceeded limit")
	}
}

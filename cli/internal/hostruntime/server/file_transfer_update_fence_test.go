package server

import (
	"net/http"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
)

// A receiver replacing its runtime answers 503 update_in_progress, which senders retry; 429 resource_limit is the
// concurrency limit and reaches the user as "destination busy" (pbvm4 update: 2 of 12 sends reported busy).
func TestUpdateFenceAnswersATransientStatusNotTheConcurrencyLimit(t *testing.T) {
	err := &filetransfer.Error{Code: filetransfer.UpdateInProgress}
	code := fileTransferErrorCode(err)
	if code != "update_in_progress" || fileTransferHTTPStatus(code) != http.StatusServiceUnavailable {
		t.Fatalf("code=%q status=%d", code, fileTransferHTTPStatus(code))
	}
	if limit := fileTransferErrorCode(&filetransfer.Error{Code: filetransfer.ResourceLimit}); fileTransferHTTPStatus(limit) != http.StatusTooManyRequests {
		t.Fatal("the concurrency limit changed status")
	}
}

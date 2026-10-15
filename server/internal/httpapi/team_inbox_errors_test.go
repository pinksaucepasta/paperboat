package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/teaminbox"
)

func TestTeamInboxLimitErrorReportsConfiguredBounds(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/team-inbox/requests", nil)
	response := httptest.NewRecorder()
	teamInboxError(response, request, &teaminbox.ManifestLimitError{MaxFileBytes: 10, MaxBatchFiles: 2, MaxBatchBytes: 12})
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				MaxFileBytes  int64 `json:"max_file_bytes"`
				MaxBatchFiles int   `json:"max_batch_files"`
				MaxBatchBytes int64 `json:"max_batch_bytes"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusRequestEntityTooLarge || body.Error.Code != "file_transfer_limit_exceeded" || body.Error.Details.MaxFileBytes != 10 || body.Error.Details.MaxBatchFiles != 2 || body.Error.Details.MaxBatchBytes != 12 {
		t.Fatal("limit response lost status, typed code or configured limits")
	}
	invalid := httptest.NewRecorder()
	teamInboxError(invalid, request, teaminbox.ErrInvalid)
	if invalid.Code != http.StatusBadRequest {
		t.Fatal("malformed metadata was misclassified as a size limit")
	}
}

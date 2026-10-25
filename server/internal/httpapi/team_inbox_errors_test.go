package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/teaminbox"
)

func TestTeamInboxLimitErrorReportsConfiguredBounds(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/team-inbox/requests", nil)
	response := httptest.NewRecorder()
	teamInboxError(response, request, &teaminbox.ManifestLimitError{MaxBatchFiles: 2})
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				MaxBatchFiles int `json:"max_batch_files"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusRequestEntityTooLarge || body.Error.Code != "file_transfer_limit_exceeded" || body.Error.Details.MaxBatchFiles != 2 {
		t.Fatal("limit response lost status, typed code or configured limits")
	}
	invalid := httptest.NewRecorder()
	teamInboxError(invalid, request, teaminbox.ErrInvalid)
	if invalid.Code != http.StatusBadRequest {
		t.Fatal("malformed metadata was misclassified as a size limit")
	}
}

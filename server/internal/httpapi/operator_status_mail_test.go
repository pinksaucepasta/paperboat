package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// A launch check reads mail_mode to find a server left in record-only mode, where no mail is sent.
func TestOperatorStatusReportsMailMode(t *testing.T) {
	for want, recordOnly := range map[string]bool{"normal": false, "record_only": true} {
		cfg := config.Default()
		cfg.Mail.RecordOnly = recordOnly
		rec := httptest.NewRecorder()
		operatorStatus(Options{Config: cfg}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/status", nil))
		var body struct {
			Data struct {
				Configuration map[string]any `json:"configuration"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK || body.Data.Configuration["mail_mode"] != want {
			t.Fatalf("record_only=%v: code=%d err=%v mail_mode=%v", recordOnly, rec.Code, err, body.Data.Configuration["mail_mode"])
		}
	}
}

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/hosthealth"
)

// The row written is exactly the source the operator guard confirmed (Operator-Target) and audited ("source"): a
// case-folded key, a repeated key, an extra field or a mismatched target is refused before any write.
func TestHostHealthReportWritesOnlyTheConfirmedSource(t *testing.T) {
	handler := hostHealthReport(hosthealth.NewService(nil))
	for _, tc := range []struct{ target, body string }{
		{"hosts", `{"source":"hosts","Source":"deploy","payload":{}}`},
		{"hosts", `{"Source":"hosts","payload":{}}`},
		{"hosts", `{"source":"deploy","source":"hosts","payload":{}}`},
		{"hosts", `{"source":"hosts","payload":{},"extra":1}`},
		{"deploy", `{"source":"hosts","payload":{}}`},
		{"hosts", `{"source":"hosts"}`},
		{"hosts", `[`},
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/admin/host-health/reports", strings.NewReader(tc.body))
		r.Header.Set("Operator-Target", tc.target)
		w := httptest.NewRecorder()
		handler(w, r) // a nil store would panic if the report reached Ingest's write
		if w.Code != 400 {
			t.Fatalf("target %s body %s = %d, want 400", tc.target, tc.body, w.Code)
		}
	}
}

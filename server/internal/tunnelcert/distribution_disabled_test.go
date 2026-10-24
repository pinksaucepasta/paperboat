package tunnelcert

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// With managed certificates disabled the edge still polls pull every few
// seconds. The route must answer the exact empty complete page the edge
// decodes, never a 404 for a known route.
func TestDisabledDistributionPullAnswersEmptyCompletePage(t *testing.T) {
	handler := DisabledDistributionHandler()
	pull := httptest.NewRecorder()
	handler.ServeHTTP(pull, httptest.NewRequest(http.MethodPost, CertificateDistributionPullPath, strings.NewReader(`{"edge_node_id":"edge-a","edge_process_epoch":"epoch-a","limit":128}`)))
	if pull.Code != http.StatusOK {
		t.Fatalf("pull status %d, want 200", pull.Code)
	}
	if got, want := strings.TrimSpace(pull.Body.String()), `{"version":1,"complete":true,"messages":[]}`; got != want {
		t.Fatalf("pull body %s, want %s", got, want)
	}
	for _, path := range []string{CertificateDistributionAckPath, CertificateDistributionRequestPath} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status %d, want 503", path, recorder.Code)
		}
	}
}

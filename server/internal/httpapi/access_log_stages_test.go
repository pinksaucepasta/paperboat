package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
)

// The access log line carries the steps a handler marks, so a slow request names where its time went.
func TestAccessLogCarriesHandlerStages(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	handler := accessLog(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverdiagnostics.Stage(r.Context(), "attach_proof")
		serverdiagnostics.Stage(r.Context(), "attach_tx")
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/runtime/carrier-attachment", nil))
	line := out.String()
	for _, field := range []string{`"msg":"http_request"`, `"attach_proof_ms":`, `"attach_tx_ms":`, `"duration_ms":`} {
		if !strings.Contains(line, field) {
			t.Fatalf("access log line %s lacks %s", line, field)
		}
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("want exactly one log line, got %q", line)
	}
}

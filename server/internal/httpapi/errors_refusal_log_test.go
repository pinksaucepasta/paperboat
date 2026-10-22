package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefusalsLogTheirCode(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(previous)
	rec := httptest.NewRecorder()
	writeError(rec, httptest.NewRequest(http.MethodPost, "/v1/machines/machine_x/exec-descriptor", nil), http.StatusConflict, "machine_not_ready", "Machine is not ready for execution.")
	if line := buf.String(); !strings.Contains(line, "http_refusal") || !strings.Contains(line, "code=machine_not_ready") || !strings.Contains(line, "status=409") {
		t.Fatalf("refusal log = %q", line)
	}
}

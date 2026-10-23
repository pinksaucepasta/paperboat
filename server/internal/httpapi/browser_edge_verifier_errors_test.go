package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
)

type browserEdgeVerifierFunc func(context.Context, *http.Request, []byte) (previewattachment.PreviewEdgeIdentity, error)

func (f browserEdgeVerifierFunc) VerifyPreviewEdgeRequest(ctx context.Context, request *http.Request, body []byte) (previewattachment.PreviewEdgeIdentity, error) {
	return f(ctx, request, body)
}

func TestBrowserEdgeVerifierSeparatesRejectionFromOperationalFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		status     int
		code       string
		cause      string
		outcome    string
		secretText string
	}{
		{
			name:    "invalid edge identity",
			err:     previewattachment.ErrUnauthorized,
			status:  http.StatusUnauthorized,
			code:    "edge_identity_invalid",
			cause:   "permission_denied",
			outcome: "rejected",
		},
		{
			name:       "verifier database failure",
			err:        errors.Join(syscall.ECONNREFUSED, errors.New("EDGE_DATABASE_SECRET")),
			status:     http.StatusServiceUnavailable,
			code:       "authorization_unavailable",
			cause:      "connection_refused",
			outcome:    "failed",
			secretText: "EDGE_DATABASE_SECRET",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, logs, reference := diagnosticRequest("/v1/edge/browser-access/begin")
			request.Method = http.MethodPost
			request.Body = io.NopCloser(strings.NewReader(`{}`))
			request.Header.Set("Content-Type", "application/json")
			request.SetPathValue("operation", "begin")
			handler := &BrowserAccessHandlers{Edge: browserEdgeVerifierFunc(func(context.Context, *http.Request, []byte) (previewattachment.PreviewEdgeIdentity, error) {
				return previewattachment.PreviewEdgeIdentity{}, tc.err
			})}
			response := httptest.NewRecorder()
			handler.EdgeRequest(response, request)
			if response.Code != tc.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if tc.secretText != "" && (strings.Contains(response.Body.String(), tc.secretText) || strings.Contains(logs.String(), tc.secretText)) {
				t.Fatalf("private verifier error leaked: body=%s logs=%s", response.Body.String(), logs.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != tc.code || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
				t.Fatalf("error response omitted safe identity: %+v", body.Error)
			}
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatalf("decode failure log: %v (%s)", err, logs.String())
			}
			for key, want := range map[string]string{
				"code":              tc.code,
				"cause":             tc.cause,
				"outcome":           tc.outcome,
				"support_reference": reference,
				"correlation_id":    "correlation_fault_01",
			} {
				if event[key] != want {
					t.Errorf("log %s=%v, want %q; log=%s", key, event[key], want, logs.String())
				}
			}
		})
	}
}

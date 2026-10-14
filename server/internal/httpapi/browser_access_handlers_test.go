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

	"github.com/pinksaucepasta/paperboat-server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat-server/internal/browseraccess"
	"github.com/pinksaucepasta/paperboat-server/internal/lazyaccess"
	"github.com/pinksaucepasta/paperboat-server/internal/previewattachment"
)

func TestBrowserAccessErrorMapsAmbiguousTeamAccess(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/browser-access/machine-credentials", nil)
	browserAccessError(recorder, request, browseraccess.ErrAmbiguousTeamAccess)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d", recorder.Code, http.StatusConflict)
	}
	var response ErrorResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != "ambiguous_team_access" || response.Error.Message != "More than one team grants access to this resource. Use a resource with one team grant until team selection is available." {
		t.Fatalf("error=%+v", response.Error)
	}
}

func TestBrowserAccessQuotaClassificationPreservesMixedOperationalFailure(t *testing.T) {
	const privateCanary = "QUOTA_OPERATION_PRIVATE_CANARY"
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "pure exhaustion", err: accountingauthority.ErrQuotaExhausted, status: http.StatusConflict, code: "bandwidth_quota_exhausted"},
		{name: "mixed storage failure", err: errors.Join(accountingauthority.ErrQuotaExhausted, syscall.EIO, errors.New(privateCanary)), status: http.StatusServiceUnavailable, code: "authorization_unavailable"},
	}
	for _, c := range []struct {
		name   string
		cause  error
		status int
		code   string
	}{
		{"invalid", browseraccess.ErrInvalid, 400, "invalid_request"},
		{"expired", browseraccess.ErrExpired, 401, "authentication_required"},
		{"redeemed", browseraccess.ErrAlreadyRedeemed, 401, "authentication_required"},
		{"ambiguous", browseraccess.ErrAmbiguousTeamAccess, 409, "ambiguous_team_access"},
		{"unauthorized", browseraccess.ErrNotAuthorized, 404, "not_found"},
		{"capacity", lazyaccess.ErrCapacity, 429, "activation_limit"},
		{"activation timeout", &lazyaccess.ActivationError{Code: "activation_timeout"}, 504, "activation_timeout"},
	} {
		tests = append(tests, struct {
			name   string
			err    error
			status int
			code   string
		}{name: c.name, err: c.cause, status: c.status, code: c.code})
		tests = append(tests, struct {
			name   string
			err    error
			status int
			code   string
		}{name: c.name + " mixed IO", err: errors.Join(c.cause, syscall.EIO, errors.New(privateCanary)), status: 503, code: "authorization_unavailable"})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, logs, reference := diagnosticRequest("/v1/browser-access/machine-credentials")
			response := httptest.NewRecorder()
			browserAccessError(response, request, test.err)
			if response.Code != test.status {
				t.Fatalf("status=%d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != test.code {
				t.Fatalf("code=%q, want %q: %+v", body.Error.Code, test.code, body.Error)
			}
			if strings.Contains(response.Body.String()+logs.String(), privateCanary) {
				t.Fatal("private error text reached the response or logs")
			}
			if test.status == http.StatusServiceUnavailable {
				var event map[string]any
				if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
					t.Fatalf("want exactly one structured fault event: %v; logs=%q", err, logs.String())
				}
				for key, want := range map[string]any{
					"stage":             "http_handler",
					"code":              "authorization_unavailable",
					"support_reference": reference,
					"correlation_id":    "correlation_fault_01",
					"errno":             float64(syscall.EIO),
					"http_status":       float64(http.StatusServiceUnavailable),
				} {
					if event[key] != want {
						t.Errorf("fault event %s=%v, want %v: %s", key, event[key], want, logs.String())
					}
				}
			}
		})
	}
}

func TestBrowserAccessBodyIOFailurePreservesOperationalCauseAndRecovers(t *testing.T) {
	const secret = "BROWSER_BODY_PRIVATE"
	calls := 0
	h := &BrowserAccessHandlers{Edge: browserEdgeVerifierFunc(func(context.Context, *http.Request, []byte) (previewattachment.PreviewEdgeIdentity, error) {
		calls++
		return previewattachment.PreviewEdgeIdentity{}, previewattachment.ErrUnauthorized
	})}
	r, logs, reference := diagnosticRequest("/v1/edge/browser-access/begin")
	r.Method = http.MethodPost
	r.Header.Set("Content-Type", "application/json")
	r.Body = diagnosticUploadReadError{err: errors.Join(syscall.EIO, errors.New(secret))}
	response := httptest.NewRecorder()
	h.EdgeRequest(response, r)
	if response.Code != 503 || calls != 0 {
		t.Fatalf("body IO status=%d authority calls=%d", response.Code, calls)
	}
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("single fault missing: %v %s", err, logs.String())
	}
	if event["errno"] != float64(syscall.EIO) || event["support_reference"] != reference || strings.Contains(response.Body.String()+logs.String(), secret) {
		t.Fatalf("body IO evidence/privacy mismatch: %v", event)
	}
	r, _, _ = diagnosticRequest("/v1/edge/browser-access/begin")
	r.Method = http.MethodPost
	r.Header.Set("Content-Type", "application/json")
	r.Body = io.NopCloser(strings.NewReader(`{}`))
	r.SetPathValue("operation", "begin")
	response = httptest.NewRecorder()
	h.EdgeRequest(response, r)
	if response.Code != 401 || calls != 1 {
		t.Fatalf("healthy body did not recover: status=%d authority calls=%d", response.Code, calls)
	}
	r, _, _ = diagnosticRequest("/v1/edge/browser-access/begin")
	r.Method = http.MethodPost
	r.Header.Set("Content-Type", "application/json")
	r.Body = io.NopCloser(strings.NewReader(strings.Repeat(" ", 16<<10+1)))
	response = httptest.NewRecorder()
	h.EdgeRequest(response, r)
	if response.Code != 400 || calls != 1 {
		t.Fatalf("body size bound changed: status=%d authority calls=%d", response.Code, calls)
	}
}

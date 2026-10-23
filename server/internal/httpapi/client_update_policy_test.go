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

	"github.com/pinksaucepasta/paperboat/server/internal/clientupdate"
)

type clientUpdatePolicyStub struct{ putErr error }

func (clientUpdatePolicyStub) Get(context.Context) (clientupdate.Policy, error) {
	return clientupdate.Policy{}, nil
}

func (s clientUpdatePolicyStub) Put(context.Context, string, string, string, clientupdate.Input) (clientupdate.Policy, error) {
	return clientupdate.Policy{}, s.putErr
}

func TestClientUpdatePolicyMixedConflictReportsOperationalFailure(t *testing.T) {
	const canary = "PRIVATE_UPDATE_POLICY_CANARY"
	request, logs, reference := diagnosticRequest("/v1/admin/client-update-policy")
	request.Method = http.MethodPut
	request.Body = io.NopCloser(strings.NewReader(`{"expected_revision":4,"minimum_version":"2026.10.8.2","reason":"support"}`))
	response := httptest.NewRecorder()
	err := errors.Join(clientupdate.ErrConflict, syscall.EIO, errors.New(canary))
	clientUpdatePolicyPut(clientUpdatePolicyStub{putErr: err})(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Error.Code != "client_update_policy_unavailable" {
		t.Fatalf("response=%s decode=%v", response.Body.String(), err)
	}
	var event struct {
		Operation        string `json:"operation"`
		Stage            string `json:"stage"`
		Code             string `json:"code"`
		Cause            string `json:"cause"`
		Errno            int    `json:"errno"`
		HTTPStatus       int    `json:"http_status"`
		SupportReference string `json:"support_reference"`
	}
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("expected one safe failure event: %v; logs=%q", err, logs.String())
	}
	if event.Operation != "http_admin" || event.Stage != "http_handler" || event.Code != "client_update_policy_unavailable" || event.Cause != "system_error" || event.Errno != int(syscall.EIO) || event.HTTPStatus != http.StatusServiceUnavailable || event.SupportReference != reference {
		t.Fatalf("fault event = %#v", event)
	}
	if strings.Contains(logs.String(), canary) || strings.Contains(response.Body.String(), canary) {
		t.Fatal("private policy failure escaped into diagnostics")
	}
}

func TestClientUpdatePolicyPureConflictRemainsConflict(t *testing.T) {
	request, _, _ := diagnosticRequest("/v1/admin/client-update-policy")
	request.Method = http.MethodPut
	request.Body = io.NopCloser(strings.NewReader(`{"expected_revision":4,"minimum_version":"2026.10.8.2","reason":"support"}`))
	response := httptest.NewRecorder()
	clientUpdatePolicyPut(clientUpdatePolicyStub{putErr: clientupdate.ErrConflict})(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestClientUpdatePolicyPublishedReleaseFailureIsReported(t *testing.T) {
	request, logs, reference := diagnosticRequest("/v1/admin/client-update-policy")
	request.Method = http.MethodPut
	request.Body = io.NopCloser(strings.NewReader(`{"expected_revision":4,"minimum_version":"2026.10.8.2","reason":"support"}`))
	response := httptest.NewRecorder()
	clientUpdatePolicyPut(clientUpdatePolicyStub{putErr: clientupdate.ErrReleaseUnavailable})(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var event struct {
		Code             string `json:"code"`
		HTTPStatus       int    `json:"http_status"`
		SupportReference string `json:"support_reference"`
	}
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("expected one release failure event: %v; logs=%q", err, logs.String())
	}
	if event.Code != "release_unavailable" || event.HTTPStatus != http.StatusServiceUnavailable || event.SupportReference != reference {
		t.Fatalf("fault event = %#v", event)
	}
}

func TestClientUpdatePolicyRejectsBodyOver4096Bytes(t *testing.T) {
	body := `{"expected_revision":4,"minimum_version":"2026.10.8.2","reason":"support"}` + strings.Repeat(" ", 4097)
	request, _, _ := diagnosticRequest("/v1/admin/client-update-policy")
	request.Method = http.MethodPut
	request.Body = io.NopCloser(strings.NewReader(body))
	response := httptest.NewRecorder()
	clientUpdatePolicyPut(clientUpdatePolicyStub{})(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized request status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestClientUpdatePolicyBodyReadFailureReportsCause(t *testing.T) {
	const canary = "PRIVATE_UPDATE_POLICY_BODY_CANARY"
	request, logs, reference := diagnosticRequest("/v1/admin/client-update-policy")
	request.Method = http.MethodPut
	request.Body = failingPolicyBody{err: errors.Join(context.Canceled, syscall.EIO, errors.New(canary))}
	response := httptest.NewRecorder()
	clientUpdatePolicyPut(clientUpdatePolicyStub{})(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Error.Code != "client_update_policy_unavailable" {
		t.Fatalf("response=%s decode=%v", response.Body.String(), err)
	}
	var event struct {
		Cause            string `json:"cause"`
		Errno            int    `json:"errno"`
		SupportReference string `json:"support_reference"`
	}
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("expected one safe failure event: %v; logs=%q", err, logs.String())
	}
	if event.Cause != "system_error" || event.Errno != int(syscall.EIO) || event.SupportReference != reference {
		t.Fatalf("fault event = %#v", event)
	}
	if strings.Contains(logs.String(), canary) || strings.Contains(response.Body.String(), canary) {
		t.Fatal("private body error escaped into diagnostics")
	}
}

func TestClientUpdatePolicyCanceledBodyReadIsQuiet(t *testing.T) {
	request, logs, _ := diagnosticRequest("/v1/admin/client-update-policy")
	request.Method = http.MethodPut
	request.Body = failingPolicyBody{err: context.Canceled}
	response := httptest.NewRecorder()
	clientUpdatePolicyPut(clientUpdatePolicyStub{})(response, request)
	if response.Body.Len() != 0 || logs.Len() != 0 {
		t.Fatalf("canceled request wrote a response or diagnostic: response=%q logs=%q", response.Body.String(), logs.String())
	}
}

type failingPolicyBody struct{ err error }

func (b failingPolicyBody) Read([]byte) (int, error) { return 0, b.err }
func (failingPolicyBody) Close() error               { return nil }

// A caller exceeding a request cap does not make an independent read failure
// disappear when the final read returns both the overflow byte and an error.
type overflowingPolicyReadFailure struct{ remaining int }

func (b *overflowingPolicyReadFailure) Read(p []byte) (int, error) {
	n := min(len(p), b.remaining)
	clear(p[:n])
	b.remaining -= n
	if b.remaining == 0 {
		return n, syscall.EIO
	}
	return n, nil
}
func (*overflowingPolicyReadFailure) Close() error { return nil }
func TestRequestBodyOverflowDoesNotHideOperationalFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		limit   int
		handler http.HandlerFunc
	}{
		{"policy", 4096, clientUpdatePolicyPut(nil)}, {"usage", 65536, resourceUsageNative(nil, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, logs, reference := diagnosticRequest("/v1/body-overflow")
			request.Method = http.MethodPut
			request.Body = &overflowingPolicyReadFailure{remaining: tc.limit + 1}
			response := httptest.NewRecorder()
			tc.handler(response, request)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("overflow hid independent read failure: status=%d", response.Code)
			}
			var event struct {
				Errno            int    `json:"errno"`
				SupportReference string `json:"support_reference"`
			}
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil || event.Errno != int(syscall.EIO) || event.SupportReference != reference {
				t.Fatal("body overflow lost original operational cause/correlation")
			}
		})
	}
}

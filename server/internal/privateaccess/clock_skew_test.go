package privateaccess

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
)

// clockSkewAnswer is the standard nested error envelope, the shape every other machine-proof endpoint answers with.
type clockSkewAnswer struct {
	Error struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
		Details   struct {
			ServerTime         string `json:"server_time"`
			AllowedSkewSeconds int    `json:"allowed_skew_seconds"`
		} `json:"details"`
	} `json:"error"`
}

// A correctly signed proof from a machine whose clock is off is a 400 clock_skew carrying the server's time, so the
// machine corrects its clock; it is not the 401 of a rejected credential. Joined with an operational error it is still a 503.
func TestMachineProofClockSkewIsA400ClockSkewOnBothMachineEndpoints(t *testing.T) {
	skew := controlplane.ErrClockSkew
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"clock skew", skew, http.StatusBadRequest},
		{"clock skew joined with I/O failure", errors.Join(skew, syscall.EIO), http.StatusServiceUnavailable},
	} {
		t.Run("routes "+tc.name, func(t *testing.T) {
			claims := controlplane.MachineRequestClaims{UserID: "acct_1", MachineID: "machine_1", InstallationGeneration: 1, OperationID: "operation_1"}
			body, _ := json.Marshal(accessorRequest{IdempotencyKey: "operation_1"})
			handler, err := NewAccessorHTTPHandler(accessorHTTPRepo{}, &fakeMachineRequest{claims: claims, err: tc.err}, fakeEdge{})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, MachineRoutesPath, bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "operation_1")
			request.Header.Set("Authorization", "Bearer machine_control_token")
			request.Header.Set("X-Paperboat-Machine-Identity", "machine_control_token")
			request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			checkClockSkewAnswer(t, response, tc.want)
		})
		t.Run("grant "+tc.name, func(t *testing.T) {
			request, _ := testPrivateTunnelTCP(testClock())
			issue := GrantIssueRequest{
				ResourceKind: request.ResourceKind, ResourceID: request.ResourceID, RouteID: request.RouteID,
				Audience: request.Audience, ExpiresAt: request.ExpiresAt, Nonce: request.Nonce,
				ConnectorID: request.ConnectorID, CarrierSessionID: request.CarrierSessionID,
				RouteGeneration: request.RouteGeneration, SessionGeneration: request.SessionGeneration,
				ProcessGeneration: request.ProcessGeneration, ConfigGeneration: request.ConfigGeneration,
				AssignmentGeneration: request.AssignmentGeneration, EdgeNodeID: request.EdgeNodeID,
				EdgeProcessEpoch: request.EdgeProcessEpoch, Protocol: request.Protocol,
				IdempotencyKey: request.IdempotencyKey, RequestID: request.RequestID, CorrelationID: request.CorrelationID,
			}
			body, _ := json.Marshal(issue)
			service, err := NewService(&fakeResolver{}, nil, Config{Idempotency: newTestIdempotency(), Now: testClock})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := NewGrantHTTPHandler(service, &fakeMachineRequest{err: tc.err})
			if err != nil {
				t.Fatal(err)
			}
			httpRequest := httptest.NewRequest(http.MethodPost, GrantPath, bytes.NewReader(body))
			httpRequest.Header.Set("Content-Type", "application/json")
			httpRequest.Header.Set("Authorization", "Bearer machine_control_token")
			httpRequest.Header.Set("X-Paperboat-Machine-Identity", "machine_control_token")
			httpRequest.Header.Set("X-Paperboat-Machine-Proof", "cHJvb2Y")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httpRequest)
			checkClockSkewAnswer(t, response, tc.want)
		})
	}
}

func checkClockSkewAnswer(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status/body=%d/%s, want %d", response.Code, response.Body.String(), want)
	}
	if want != http.StatusBadRequest {
		return
	}
	var answer clockSkewAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || answer.Error.Code != "clock_skew" || !answer.Error.Retryable ||
		answer.Error.Details.ServerTime == "" || answer.Error.Details.AllowedSkewSeconds <= 0 {
		t.Fatalf("clock skew answer = %s (%v)", response.Body.String(), err)
	}
}

// Every private-access refusal uses the same nested envelope, not a second flat shape clients would have to learn.
func TestPrivateAccessErrorsUseTheStandardNestedEnvelope(t *testing.T) {
	response := httptest.NewRecorder()
	writeHTTPError(context.Background(), response, http.StatusUnauthorized, "machine_identity_invalid", "Machine authentication is required.", false)
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != "machine_identity_invalid" || body.Error.Message == "" || body.Code != "" {
		t.Fatalf("answer = %s (%v)", response.Body.String(), err)
	}
}

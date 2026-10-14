package httpapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/environment"
)

type layerObservationTestService struct{ calls int }

func (s *layerObservationTestService) RecordVaultLayerObservations(_ context.Context, account, machine string, in environment.VaultLayerObservationsPut) (environment.VaultLayerObservationsAck, error) {
	s.calls++
	return environment.VaultLayerObservationsAck{OperationID: in.OperationID, Accepted: true}, nil
}

type layerObservationTestVerifier struct {
	claims controlplane.MachineRequestClaims
	body   string
	calls  int
}

func (v *layerObservationTestVerifier) VerifyMachineRequest(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error) {
	panic("helper-capable verifier must not be used")
}
func (v *layerObservationTestVerifier) VerifyMachineControlRequest(_ context.Context, _ string, _ []byte, _ string, _ string, body []byte) (controlplane.MachineRequestClaims, error) {
	v.calls++
	v.body = string(body)
	return v.claims, nil
}
func TestLayerObservationRouteRejectsHumanForeignAndUnsignedOperation(t *testing.T) {
	for _, which := range []string{"human", "foreign", "operation", "unknown_field", "valid"} {
		t.Run(which, func(t *testing.T) {
			service := &layerObservationTestService{}
			verifier := &layerObservationTestVerifier{claims: controlplane.MachineRequestClaims{UserID: "owner", MachineID: "host", OperationID: "operation"}}
			mux := http.NewServeMux()
			registerEnvironmentVaultLayerObservationRoute(mux, service, verifier)
			body := `{"operation_id":"operation","observations":[]}`
			if which == "foreign" {
				verifier.claims.MachineID = "foreign"
			}
			if which == "operation" {
				verifier.claims.OperationID = "other"
			}
			if which == "unknown_field" {
				body = `{"operation_id":"operation","observations":[],"private":"forbidden"}`
			}
			req := httptest.NewRequest("POST", "/v1/environment/hosts/host/layer-observations", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer identity")
			if which != "human" {
				req.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
			}
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if which == "valid" {
				if rr.Code != 200 || service.calls != 1 || verifier.body != body {
					t.Fatalf("signed report not accepted: status %d calls %d", rr.Code, service.calls)
				}
			} else if rr.Code < 400 || service.calls != 0 {
				t.Fatalf("unauthorized report reached storage: status %d calls %d", rr.Code, service.calls)
			}
		})
	}
}

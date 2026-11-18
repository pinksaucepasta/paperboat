package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelstore"
	"github.com/pinksaucepasta/paperboat/server/internal/ratelimit"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

type tunnelAPIStub struct {
	createResult    tunnelv1.MutationResult
	createErr       error
	patchResult     tunnelv1.MutationResult
	patchErr        error
	stateResult     tunnelv1.MutationResult
	stateErr        error
	createReq       previewtunnelapi.RequestContext
	createWorkspace workspace.Scope
	createSelected  bool
	patchReq        tunnelv1.PatchTunnelRequest
	check           tunnelv1.SubdomainCheck
	checkErr        error
	checkedLabel    string
	mutationReq     previewtunnelapi.RequestContext
	listCalls       int
	listFilters     workspace.InventoryFilters
}

func (s *tunnelAPIStub) CreateTunnel(ctx context.Context, request previewtunnelapi.RequestContext, _ tunnelv1.CreateTunnelRequest) (tunnelv1.MutationResult, error) {
	s.createReq = request
	s.createWorkspace, s.createSelected = workspace.FromContext(ctx)
	return s.createResult, s.createErr
}

func (s *tunnelAPIStub) ListTunnels(ctx context.Context, _ previewtunnelapi.RequestContext, _ string, _ int) (tunnelv1.TunnelPage, error) {
	s.listCalls++
	s.listFilters = workspace.InventoryFiltersFromContext(ctx)
	return tunnelv1.TunnelPage{}, nil
}

func (s *tunnelAPIStub) GetTunnel(context.Context, previewtunnelapi.RequestContext, string) (tunnelv1.TunnelView, error) {
	return tunnelv1.TunnelView{}, nil
}

func (s *tunnelAPIStub) CheckSubdomain(_ context.Context, _ previewtunnelapi.RequestContext, _, label string) (tunnelv1.SubdomainCheck, error) {
	s.checkedLabel = label
	return s.check, s.checkErr
}

func (s *tunnelAPIStub) PatchTunnel(_ context.Context, request previewtunnelapi.RequestContext, _ string, input tunnelv1.PatchTunnelRequest) (tunnelv1.MutationResult, error) {
	s.patchReq = input
	s.patchReq.MutationInput = input.MutationInput
	s.patchReq.Name = input.Name
	s.patchReq.AccessMode = input.AccessMode
	s.patchReq.Subdomain = input.Subdomain
	s.patchReq.ExpiresAt = input.ExpiresAt
	s.mutationReq = request
	return s.patchResult, s.patchErr
}

func (s *tunnelAPIStub) PauseTunnel(_ context.Context, request previewtunnelapi.RequestContext, _ string, _ tunnelv1.MutationInput) (tunnelv1.MutationResult, error) {
	s.mutationReq = request
	return s.stateResult, s.stateErr
}

func (s *tunnelAPIStub) ResumeTunnel(_ context.Context, request previewtunnelapi.RequestContext, _ string, _ tunnelv1.MutationInput) (tunnelv1.MutationResult, error) {
	s.mutationReq = request
	return s.stateResult, s.stateErr
}

func (s *tunnelAPIStub) DeleteTunnel(_ context.Context, request previewtunnelapi.RequestContext, _ string, _ tunnelv1.MutationInput) (tunnelv1.MutationResult, error) {
	s.mutationReq = request
	return s.stateResult, s.stateErr
}

func (s *tunnelAPIStub) Status(context.Context, previewtunnelapi.RequestContext, string) (tunnelv1.HealthView, error) {
	return tunnelv1.HealthView{}, nil
}

type tunnelIdentityVerifier struct {
	claims       controlplane.MachineRequestClaims
	err          error
	called       bool
	method, path string // the signed request; default POST /v1/tunnels
	body         []byte
}

func (v *tunnelIdentityVerifier) VerifyMachineRequest(_ context.Context, identity string, proof []byte, method, path string, body []byte) (controlplane.MachineRequestClaims, error) {
	v.called = true
	wantMethod, wantPath := http.MethodPost, "/v1/tunnels"
	if v.method != "" {
		wantMethod, wantPath = v.method, v.path
	}
	v.body = body
	if identity != "signed-machine-identity" || string(proof) != "signed-proof" || method != wantMethod || path != wantPath || (v.method == "" && len(body) == 0) {
		return controlplane.MachineRequestClaims{}, errors.New("unexpected verifier input")
	}
	return v.claims, v.err
}

func tunnelHandlerRequest(method, path string, body []byte) *http.Request {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	client := auth.ClientPrincipal{SessionID: "cli_1", Scopes: []string{"tunnels:read", "tunnels:write"}}
	ctx := context.WithValue(request.Context(), authContextKey{}, principal{
		User: auth.User{ID: "acct_1", Role: auth.RoleUser}, Client: &client,
	})
	ctx = serverdiagnostics.WithRequestID(ctx, "req_1")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "corr_1")
	return request.WithContext(ctx)
}

func tunnelMachineHandlerRequest(method, path string, body []byte) *http.Request {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	ctx := serverdiagnostics.WithRequestID(request.Context(), "req_1")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "corr_1")
	return request.WithContext(ctx)
}

func tunnelMutationResult(state, phase string) tunnelv1.MutationResult {
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	return tunnelv1.MutationResult{
		Tunnel: tunnelv1.TunnelView{
			Schema: tunnelv1.Schema, Kind: "tunnel", ID: "tun_1", AccountID: "acct_1", Name: "demo",
			DesiredState: tunnelv1.DesiredActive, AccessMode: tunnelv1.AccessPublic, Generation: 1,
			ETag: previewtunnelapi.ETag("tunnel", "tun_1", 1), StableEndpointID: "endpoint_11111111-1111-4111-8111-111111111111",
			StableEndpoint: "https://11111111-1111-4111-8111-111111111111.tunnels.example.test", CreatedByHostID: "machine_1",
			CreatedByActorID: "acct_1", SummaryCode: "pending", CreatedAt: now, UpdatedAt: now,
		},
		Operation: previewtunnelapi.Operation{
			Schema: tunnelv1.Schema, Kind: "operation", ID: "op_1", ResourceKind: "tunnel", ResourceID: "tun_1",
			Phase: phase, State: state, Progress: 40, CorrelationID: "corr_1", CreatedAt: now, UpdatedAt: now,
		},
	}
}

func decodeTunnelHandlerBody(t *testing.T, response *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var envelope struct {
		Data  map[string]json.RawMessage `json:"data"`
		Error map[string]json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not JSON: %v; body=%s", err, response.Body.String())
	}
	if envelope.Data != nil {
		return envelope.Data
	}
	return envelope.Error
}

func TestTunnelCreateUsesSignedMachineIdentityAndOperationBinding(t *testing.T) {
	body := []byte(`{"name":"demo","origin":{"scheme":"http","address":"127.0.0.1:3000"}}`)
	service := &tunnelAPIStub{createResult: tunnelMutationResult("running", "connecting")}
	verifier := &tunnelIdentityVerifier{claims: controlplane.MachineRequestClaims{
		UserID: "acct_1", MachineID: "machine_1", InstallationGeneration: 3, OperationID: "op_create_1",
	}}
	request := tunnelMachineHandlerRequest(http.MethodPost, "/v1/tunnels", body)
	request.Header.Set("Idempotency-Key", "op_create_1")
	request.Header.Set("Authorization", "Bearer signed-machine-identity")
	request.Header.Set("X-Paperboat-Machine-Identity", "signed-machine-identity")
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("signed-proof")))
	response := httptest.NewRecorder()
	tunnelCreate(service, verifier).ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !verifier.called {
		t.Fatalf("status=%d verifier_called=%v body=%s", response.Code, verifier.called, response.Body.String())
	}
	if service.createReq.Actor.AccountID != "acct_1" || service.createReq.Actor.ActorID != "acct_1" || service.createReq.Actor.MachineID != "machine_1" || service.createReq.Actor.HostID != "machine_1" || service.createReq.Actor.Role != "user" || len(service.createReq.Actor.Scopes) != 1 || service.createReq.Actor.Scopes[0] != "tunnels:write" {
		t.Fatalf("host identity was not derived from signed claims: %+v", service.createReq.Actor)
	}
	data := decodeTunnelHandlerBody(t, response)
	var kind string
	if err := json.Unmarshal(data["kind"], &kind); err != nil || kind != "operation" {
		t.Fatalf("running mutation wire resource kind = %q, body=%s", kind, response.Body.String())
	}
	if _, exposed := data["replayed"]; exposed {
		t.Fatalf("internal replay field leaked: %s", response.Body.String())
	}
}

func TestTunnelCreateRejectsProofReplayUnderDifferentIdempotencyKey(t *testing.T) {
	body := []byte(`{"name":"demo","origin":{"scheme":"http","address":"127.0.0.1:3000"}}`)
	service := &tunnelAPIStub{createResult: tunnelMutationResult("running", "connecting")}
	verifier := &tunnelIdentityVerifier{claims: controlplane.MachineRequestClaims{
		UserID: "acct_1", MachineID: "machine_1", InstallationGeneration: 3, OperationID: "op_signed",
	}}
	request := tunnelMachineHandlerRequest(http.MethodPost, "/v1/tunnels", body)
	request.Header.Set("Idempotency-Key", "op_different")
	request.Header.Set("Authorization", "Bearer signed-machine-identity")
	request.Header.Set("X-Paperboat-Machine-Identity", "signed-machine-identity")
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("signed-proof")))
	response := httptest.NewRecorder()
	tunnelCreate(service, verifier).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || service.createReq.Actor.AccountID != "" {
		t.Fatalf("replayed proof status=%d request=%+v body=%s", response.Code, service.createReq, response.Body.String())
	}
	var payload ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Error.Code != "machine_identity_invalid" {
		t.Fatalf("replay error=%+v body=%s", payload.Error, response.Body.String())
	}
}

func TestTunnelCreateRejectsMissingOrInvalidMachineProof(t *testing.T) {
	body := []byte(`{"name":"demo","origin":{"scheme":"http","address":"127.0.0.1:3000"}}`)
	for _, test := range []struct {
		name  string
		proof string
	}{
		{name: "missing", proof: ""},
		{name: "invalid encoding", proof: "not-base64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &tunnelAPIStub{createResult: tunnelMutationResult("running", "connecting")}
			verifier := &tunnelIdentityVerifier{claims: controlplane.MachineRequestClaims{
				UserID: "acct_1", MachineID: "machine_1", InstallationGeneration: 3, OperationID: "op_create_1",
			}}
			request := tunnelMachineHandlerRequest(http.MethodPost, "/v1/tunnels", body)
			request.Header.Set("Idempotency-Key", "op_create_1")
			request.Header.Set("Authorization", "Bearer signed-machine-identity")
			request.Header.Set("X-Paperboat-Machine-Identity", "signed-machine-identity")
			if test.proof != "" {
				request.Header.Set("X-Paperboat-Machine-Proof", test.proof)
			}
			response := httptest.NewRecorder()
			tunnelCreate(service, verifier).ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized || verifier.called || service.createReq.Actor.AccountID != "" {
				t.Fatalf("status=%d verifier_called=%v request=%+v body=%s", response.Code, verifier.called, service.createReq, response.Body.String())
			}
			var payload ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Error.Code != "machine_identity_invalid" {
				t.Fatalf("error=%+v body=%s", payload.Error, response.Body.String())
			}
		})
	}
}

func TestTunnelCreateRejectsAuthenticatedClientWithoutMachineProof(t *testing.T) {
	service := &tunnelAPIStub{createResult: tunnelMutationResult("running", "connecting")}
	request := tunnelHandlerRequest(http.MethodPost, "/v1/tunnels", []byte(`{"name":"demo","origin":{"scheme":"http","address":"127.0.0.1:3000"}}`))
	request.Header.Set("Idempotency-Key", "op_create_1")
	response := httptest.NewRecorder()
	tunnelCreate(service, &tunnelIdentityVerifier{}).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || service.createReq.Actor.AccountID != "" {
		t.Fatalf("status=%d request=%+v body=%s", response.Code, service.createReq, response.Body.String())
	}
	var payload ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Error.Code != "machine_identity_required" {
		t.Fatalf("error=%+v body=%s", payload.Error, response.Body.String())
	}
}

func TestTunnelMalformedDocumentsReturnTypedBadRequest(t *testing.T) {
	service := &tunnelAPIStub{}
	for _, test := range []struct {
		name    string
		handler http.Handler
		path    string
		body    string
	}{
		{name: "create unknown field", handler: tunnelCreate(service, &tunnelIdentityVerifier{}), path: "/v1/tunnels", body: `{"name":"demo","unexpected":true}`},
		{name: "patch unknown field", handler: tunnelPatch(service, &tunnelIdentityVerifier{}, nil), path: "/v1/tunnels/tun_1", body: `{"unexpected":true}`},
		{name: "state nonempty", handler: tunnelPause(service, &tunnelIdentityVerifier{}), path: "/v1/tunnels/tun_1/pause", body: `{"unexpected":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := tunnelHandlerRequest(http.MethodPost, test.path, []byte(test.body))
			if test.name == "patch unknown field" {
				request.Method = http.MethodPatch
			}
			response := httptest.NewRecorder()
			test.handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var payload ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Error.Code != "invalid_request" {
				t.Fatalf("typed error=%+v body=%s", payload.Error, response.Body.String())
			}
		})
	}
}

func TestTunnelPatchDistinguishesExplicitExpiryRemovalAndReturnsCanonicalOperation(t *testing.T) {
	service := &tunnelAPIStub{patchResult: tunnelMutationResult("running", "connecting")}
	request := tunnelHandlerRequest(http.MethodPatch, "/v1/tunnels/tun_1", []byte(`{"expires_at":null}`))
	request.SetPathValue("tunnel_id", "tun_1")
	request.Header.Set("If-Match", previewtunnelapi.ETag("tunnel", "tun_1", 1))
	request.Header.Set("Idempotency-Key", "op_patch_1")
	response := httptest.NewRecorder()
	tunnelPatch(service, &tunnelIdentityVerifier{}, nil).ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !service.patchReq.ExpirySet || service.patchReq.ExpiresAt != nil {
		t.Fatalf("status=%d patch=%+v body=%s", response.Code, service.patchReq, response.Body.String())
	}
	data := decodeTunnelHandlerBody(t, response)
	var kind string
	if err := json.Unmarshal(data["kind"], &kind); err != nil || kind != "operation" {
		t.Fatalf("wire kind=%q body=%s", kind, response.Body.String())
	}
}

func TestTunnelCreateBindsSignedWorkspace(t *testing.T) {
	for _, test := range []struct {
		name, query, field string
		want               int
		selected           bool
	}{
		{"body", "", `"workspace":"alpha",`, http.StatusAccepted, true},
		{"matching", "?workspace=alpha", `"workspace":"alpha",`, http.StatusAccepted, true},
		{"query-only", "?workspace=alpha", "", http.StatusBadRequest, false},
		{"null", "", `"workspace":null,`, http.StatusBadRequest, false},
		{"mismatch", "?workspace=beta", `"workspace":"alpha",`, http.StatusBadRequest, false},
		{"unselected-runtime", "", "", http.StatusAccepted, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{` + test.field + `"name":"demo","origin":{"scheme":"http","address":"127.0.0.1:3000"}}`)
			service := &tunnelAPIStub{createResult: tunnelMutationResult("running", "connecting")}
			verifier := &tunnelIdentityVerifier{claims: controlplane.MachineRequestClaims{UserID: "acct_1", MachineID: "machine_1", InstallationGeneration: 1, OperationID: "op_create_1"}}
			request := tunnelMachineHandlerRequest(http.MethodPost, "/v1/tunnels"+test.query, body)
			request.Header.Set("Authorization", "Bearer signed-machine-identity")
			request.Header.Set("X-Paperboat-Machine-Identity", "signed-machine-identity")
			request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("signed-proof")))
			request.Header.Set("Idempotency-Key", "op_create_1")
			recorder := httptest.NewRecorder()
			tunnelCreate(service, verifier).ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if service.createSelected != test.selected {
				t.Fatalf("selection=%+v selected=%v", service.createWorkspace, service.createSelected)
			}
			if test.selected && service.createWorkspace.ID != "alpha" {
				t.Fatal("signed workspace lost")
			}
		})
	}
}

func TestPublisherTeamSubscriptionRequiredErrors(t *testing.T) {
	for name, write := range map[string]func(http.ResponseWriter, *http.Request, error){"tunnel": writeTunnelError, "preview": writePreviewLeaseError} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			write(response, httptest.NewRequest(http.MethodPost, "/v1/"+name+"s", nil), previewtunnelstore.ErrTeamSubscriptionRequired)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var result struct {
				Error struct {
					Code         string `json:"code"`
					Outcome      string `json:"outcome"`
					Retryable    bool   `json:"retryable"`
					RepairAction string `json:"repair_action"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Error.Code != "team_subscription_required" || result.Error.Outcome != "unchanged" || result.Error.Retryable || result.Error.RepairAction != "manage_team_billing" {
				t.Fatalf("error=%+v", result.Error)
			}
		})
	}
}

func TestTunnelListDesiredStateFilter(t *testing.T) {
	for _, tc := range []struct {
		state  string
		status int
	}{{"active", 200}, {"paused", 200}, {"deleted", 200}, {"stopped", 400}} {
		t.Run(tc.state, func(t *testing.T) {
			service := &tunnelAPIStub{}
			recorder := httptest.NewRecorder()
			tunnelList(service).ServeHTTP(recorder, tunnelHandlerRequest(http.MethodGet, "/v1/tunnels?state="+tc.state+"&limit=20", nil))
			if recorder.Code != tc.status {
				t.Fatalf("state=%s status=%d body=%s", tc.state, recorder.Code, recorder.Body.String())
			}
			if tc.status == http.StatusOK {
				if service.listCalls != 1 || service.listFilters.State != tc.state {
					t.Fatalf("accepted state not forwarded: calls=%d filters=%+v", service.listCalls, service.listFilters)
				}
			} else if service.listCalls != 0 {
				t.Fatal("invalid state reached service")
			}
		})
	}
}

// TestTunnelMutationsAcceptHostMachineProof: a recipe machine has no bearer client session, so patch, pause, resume
// and delete take the host actor from its signed proof (the service then limits it to tunnels that host created),
// and a bad proof fails closed.
func TestTunnelMutationsAcceptHostMachineProof(t *testing.T) {
	for _, test := range []struct {
		method, path, body string
		handler            func(tunnelv1.API, machineRequestVerifier) http.HandlerFunc
	}{
		{http.MethodPatch, "/v1/tunnels/tun_1", `{"name":"renamed"}`, func(service tunnelv1.API, identities machineRequestVerifier) http.HandlerFunc {
			return tunnelPatch(service, identities, nil)
		}},
		{http.MethodPost, "/v1/tunnels/tun_1/pause", ``, tunnelPause},
		{http.MethodPost, "/v1/tunnels/tun_1/resume", `{}`, tunnelResume},
		{http.MethodDelete, "/v1/tunnels/tun_1", ``, tunnelDelete},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			for _, operation := range []string{"op_mutate_1", "op_other"} { // op_other: a proof replayed under another key
				service := &tunnelAPIStub{patchResult: tunnelMutationResult("running", "connecting"), stateResult: tunnelMutationResult("running", "connecting")}
				verifier := &tunnelIdentityVerifier{method: test.method, path: test.path, claims: controlplane.MachineRequestClaims{
					UserID: "acct_1", MachineID: "machine_1", InstallationGeneration: 3, OperationID: operation,
				}}
				request := tunnelMachineHandlerRequest(test.method, test.path, []byte(test.body))
				request.SetPathValue("tunnel_id", "tun_1")
				request.Header.Set("If-Match", previewtunnelapi.ETag("tunnel", "tun_1", 1))
				request.Header.Set("Idempotency-Key", "op_mutate_1")
				request.Header.Set("Authorization", "Bearer signed-machine-identity")
				request.Header.Set("X-Paperboat-Machine-Identity", "signed-machine-identity")
				request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("signed-proof")))
				response := httptest.NewRecorder()
				test.handler(service, verifier).ServeHTTP(response, request)
				if !verifier.called || string(verifier.body) != test.body {
					t.Fatalf("proof not verified over the sent body: called=%v body=%q", verifier.called, verifier.body)
				}
				if operation != "op_mutate_1" {
					if response.Code != http.StatusUnauthorized || service.mutationReq.Actor.HostID != "" {
						t.Fatalf("bad proof: status=%d actor=%+v body=%s", response.Code, service.mutationReq.Actor, response.Body.String())
					}
					continue
				}
				actor := service.mutationReq.Actor
				if response.Code != http.StatusAccepted || actor.AccountID != "acct_1" || actor.HostID != "machine_1" || actor.MachineID != "machine_1" {
					t.Fatalf("status=%d actor=%+v body=%s", response.Code, actor, response.Body.String())
				}
			}
		})
	}
}

func TestTunnelPatchSubdomainReachesTheServiceAndIsRateLimitedPerAccount(t *testing.T) {
	service := &tunnelAPIStub{patchResult: tunnelMutationResult("running", "connecting")}
	limiter := ratelimit.New(1, 1, 1000)
	patch := func(body string) *httptest.ResponseRecorder {
		request := tunnelHandlerRequest(http.MethodPatch, "/v1/tunnels/tun_1", []byte(body))
		request.SetPathValue("tunnel_id", "tun_1")
		request.Header.Set("If-Match", previewtunnelapi.ETag("tunnel", "tun_1", 1))
		request.Header.Set("Idempotency-Key", "op_patch_1")
		response := httptest.NewRecorder()
		tunnelPatch(service, &tunnelIdentityVerifier{}, limiter).ServeHTTP(response, request)
		return response
	}
	if response := patch(`{"subdomain":"helloworld"}`); response.Code != http.StatusAccepted || service.patchReq.Subdomain == nil || *service.patchReq.Subdomain != "helloworld" {
		t.Fatalf("status=%d patch=%+v body=%s", response.Code, service.patchReq, response.Body.String())
	}
	if response := patch(`{"subdomain":"another"}`); response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("second change: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := patch(`{"name":"renamed"}`); response.Code != http.StatusAccepted {
		t.Fatalf("a label change must not use the address limit: status=%d", response.Code)
	}
	if response := patch(`{"subdomain":null}`); response.Code != http.StatusBadRequest {
		t.Fatalf("null subdomain: status=%d", response.Code)
	}
}

func TestTunnelErrorsNameTheSubdomainProblem(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{tunnelv1.ErrSubdomainTaken, http.StatusConflict, "subdomain_taken"},
		{tunnelv1.ErrSubdomainReserved, http.StatusBadRequest, "subdomain_reserved"},
		{fmt.Errorf("%w: use 3 to 63 lowercase letters", tunnelv1.ErrSubdomainInvalid), http.StatusBadRequest, "invalid_subdomain"},
		{tunnelv1.ErrAddressBlocked, http.StatusForbidden, "address_blocked"},
		{tunnelv1.ErrAddressUnavailable, http.StatusBadRequest, "address_not_available"},
		{tunnelv1.ErrAddressChangePending, http.StatusConflict, "address_change_pending"},
	} {
		service := &tunnelAPIStub{patchErr: tc.err}
		request := tunnelHandlerRequest(http.MethodPatch, "/v1/tunnels/tun_1", []byte(`{"subdomain":"helloworld"}`))
		request.SetPathValue("tunnel_id", "tun_1")
		request.Header.Set("If-Match", previewtunnelapi.ETag("tunnel", "tun_1", 1))
		request.Header.Set("Idempotency-Key", "op_patch_1")
		response := httptest.NewRecorder()
		tunnelPatch(service, &tunnelIdentityVerifier{}, nil).ServeHTTP(response, request)
		var payload ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || response.Code != tc.status || payload.Error.Code != tc.code {
			t.Errorf("%v: status=%d code=%q body=%s", tc.err, response.Code, payload.Error.Code, response.Body.String())
		}
	}
}

func TestTunnelSubdomainAvailabilityAnswersAndIsRateLimited(t *testing.T) {
	service := &tunnelAPIStub{check: tunnelv1.SubdomainCheck{Subdomain: "helloworld", Available: false, Reason: "taken", Message: "This name is already taken."}}
	limiter := ratelimit.New(1, 1, 1000)
	get := func() *httptest.ResponseRecorder {
		request := tunnelHandlerRequest(http.MethodGet, "/v1/tunnels/tun_1/subdomain-availability?subdomain=helloworld", nil)
		request.SetPathValue("tunnel_id", "tun_1")
		response := httptest.NewRecorder()
		tunnelSubdomainAvailability(service, limiter).ServeHTTP(response, request)
		return response
	}
	response := get()
	data := decodeTunnelHandlerBody(t, response)
	var reason string
	if err := json.Unmarshal(data["reason"], &reason); err != nil || response.Code != http.StatusOK || reason != "taken" || service.checkedLabel != "helloworld" {
		t.Fatalf("status=%d reason=%q label=%q body=%s", response.Code, reason, service.checkedLabel, response.Body.String())
	}
	if again := get(); again.Code != http.StatusTooManyRequests {
		t.Fatalf("second check: status=%d", again.Code)
	}
	// A preview's ID (or any non-tunnel) is just an unknown tunnel here: previews have no address to check or change.
	service.checkErr = tunnelv1.ErrNotFound
	request := tunnelHandlerRequest(http.MethodGet, "/v1/tunnels/prv_1/subdomain-availability?subdomain=helloworld", nil)
	request.SetPathValue("tunnel_id", "prv_1")
	response = httptest.NewRecorder()
	tunnelSubdomainAvailability(service, nil).ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown tunnel: status=%d body=%s", response.Code, response.Body.String())
	}
}

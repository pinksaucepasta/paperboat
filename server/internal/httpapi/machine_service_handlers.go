package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat-server/internal/nativeprivateaccess"
)

func machineServiceError(w http.ResponseWriter, r *http.Request, e error) {
	switch {
	case expectedHTTPErrorOnly(e, wire.ErrInvalid):
		writeError(w, r, 400, "invalid_request", "Machine service request is invalid.")
	case expectedHTTPErrorOnly(e, machineservices.ErrConflict):
		writeError(w, r, 409, "generation_conflict", "Machine service policy changed; refresh and retry.")
	case expectedHTTPErrorOnly(e, machineservices.ErrDenied, nativeprivateaccess.ErrDenied):
		writeError(w, r, 403, "forbidden", "Machine service access is not allowed.")
	default:
		writeError(w, r, 503, "machine_services_unavailable", "Machine services are temporarily unavailable.", e)
	}
}
func machineServicePolicy(s *machineservices.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		var result wire.Policy
		var e error
		if r.Method == http.MethodPut {
			var in wire.PolicyUpdate
			d := json.NewDecoder(io.LimitReader(r.Body, 4097))
			d.DisallowUnknownFields()
			if d.Decode(&in) != nil || d.Decode(&struct{}{}) != io.EOF {
				machineServiceError(w, r, wire.ErrInvalid)
				return
			}
			result, e = s.UpdatePolicy(r.Context(), p.User.ID, r.PathValue("machine_id"), in)
		} else {
			result, e = s.GetPolicy(r.Context(), p.User.ID, r.PathValue("machine_id"))
		}
		if e != nil {
			machineServiceError(w, r, e)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: result})
	}
}
func machineServiceList(s *machineservices.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		items, e := s.List(r.Context(), p.User.ID)
		if e != nil {
			machineServiceError(w, r, e)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"items": items}})
	}
}
func runtimeMachineServiceAuthorize(identities machineRequestVerifier, s *nativeprivateaccess.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		var in struct {
			EnvironmentID      string `json:"environment_id"`
			ResourceID         string `json:"resource_id"`
			UserID             string `json:"user_id"`
			CLIClientSessionID string `json:"cli_client_session_id"`
			AccessSessionID    string `json:"access_session_id"`
			Expected           struct {
				Schema                 string    `json:"schema"`
				ResourceKind           string    `json:"resource_kind"`
				ResourceID             string    `json:"resource_id"`
				ResourceGeneration     int64     `json:"resource_generation"`
				RouteID                string    `json:"route_id"`
				RouteGeneration        int64     `json:"route_generation"`
				TargetGeneration       int64     `json:"target_generation"`
				OwnerEndpointID        string    `json:"owner_endpoint_id"`
				Protocol               string    `json:"protocol"`
				TargetScheme           string    `json:"target_scheme"`
				TargetAddress          string    `json:"target_address"`
				ExpiresAt              time.Time `json:"expires_at"`
				InstallationGeneration int64     `json:"installation_generation"`
				BootID                 string    `json:"boot_id"`
				PolicyGeneration       int64     `json:"policy_generation"`
				AnnouncementGeneration int64     `json:"announcement_generation"`
			} `json:"expected"`
		}
		body, e := io.ReadAll(io.LimitReader(r.Body, 8193))
		d := json.NewDecoder(bytes.NewReader(body))
		d.DisallowUnknownFields()
		if e != nil || len(body) > 8192 || d.Decode(&in) != nil || d.Decode(&struct{}{}) != io.EOF {
			machineServiceError(w, r, wire.ErrInvalid)
			return
		}
		proof, e := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if e != nil || len(proof) == 0 || identities == nil {
			writeError(w, r, 401, "unauthenticated", "Current signed runtime identity is required.")
			return
		}
		identity, e := identities.VerifyMachineRequest(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), proof, r.Method, r.URL.Path, body)
		if e != nil {
			writeMachineProofFailure(w, r, e, "unauthenticated", "Current signed runtime identity is required.")
			return
		}
		if identity.EnvironmentID != in.EnvironmentID || identity.MachineID != in.ResourceID {
			logHTTPRejection(r, http.StatusUnauthorized, "unauthenticated", nil)
			writeError(w, r, 401, "unauthenticated", "Current signed runtime identity is required.")
			return
		}
		x := in.Expected
		if x.Schema != "paperboat.native-private-target/v1" || x.ResourceKind != "machine_service" || x.ResourceID != in.ResourceID || x.OwnerEndpointID != in.ResourceID || x.ExpiresAt.IsZero() {
			machineServiceError(w, r, machineservices.ErrDenied)
			return
		}
		resolver, ok := s.Resolver.(*nativeprivateaccess.SQLResolver)
		if !ok {
			machineServiceError(w, r, errors.New("resolver unavailable"))
			return
		}
		expiry, e := resolver.MachineServiceCurrent(r.Context(), nativeprivateaccess.Request{AccountID: in.UserID, UserID: in.UserID, CLIClientSessionID: in.CLIClientSessionID, ResourceKind: "machine_service", ResourceID: in.ResourceID, RouteID: x.RouteID, Protocol: x.Protocol}, in.AccessSessionID, nativeprivateaccess.Target{EnvironmentID: in.EnvironmentID, ResourceID: x.ResourceID, ResourceGeneration: x.ResourceGeneration, RouteID: x.RouteID, RouteGeneration: x.RouteGeneration, TargetGeneration: x.TargetGeneration, Protocol: x.Protocol, TargetScheme: x.TargetScheme, TargetAddress: x.TargetAddress, InstallationGeneration: x.InstallationGeneration, BootID: x.BootID, PolicyGeneration: x.PolicyGeneration, AnnouncementGeneration: x.AnnouncementGeneration})
		if e != nil {
			machineServiceError(w, r, e)
			return
		}
		cap := time.Now().UTC().Add(machineservices.SnapshotLifetime)
		if expiry.After(cap) {
			expiry = cap
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"expires_at": expiry}})
	}
}

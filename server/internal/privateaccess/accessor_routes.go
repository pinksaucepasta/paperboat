package privateaccess

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/canonicaljson"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

const (
	MachineRoutesPath  = "/v1/private-access/routes"
	EdgeAdmissionsPath = "/v1/edge/private-access/carrier-admissions"
)

// AccessorAdmission is a short-lived, secret-free projection. It authorizes one enrolled machine identity
// to establish a carrier to one exact edge and target generation. It is not derived from preview ownership.
type AccessorAdmission = connectorprotocol.PrivateAccessAdmission

type AccessorSnapshot = connectorprotocol.PrivateAccessSnapshot

type accessorRows interface {
	ListPrivateAccessRoutesForMachineV1(context.Context, dbsqlc.ListPrivateAccessRoutesForMachineV1Params) ([]dbsqlc.ListPrivateAccessRoutesForMachineV1Row, error)
	ListPrivateAccessCarrierAdmissionsForEdgeV1(context.Context, dbsqlc.ListPrivateAccessCarrierAdmissionsForEdgeV1Params) ([]dbsqlc.ListPrivateAccessCarrierAdmissionsForEdgeV1Row, error)
}

type AccessorRepository struct {
	queries accessorRows
	now     func() time.Time
}

func NewAccessorRepository(database *db.DB) (*AccessorRepository, error) {
	if database == nil || database.Pool() == nil {
		return nil, fmt.Errorf("%w: accessor database is unavailable", ErrInvalid)
	}
	return &AccessorRepository{queries: dbsqlc.New(database.Pool()), now: func() time.Time { return time.Now().UTC() }}, nil
}

func admissionFromValues(now time.Time, accountID, resourceKind, resourceID, tunnelName, routeName, operationID, connectorID, carrierSessionID, routeID string, routeGeneration, sessionGeneration, processGeneration, configGeneration, assignmentGeneration int64, edgeNodeID, edgeEpoch, protocol, hostname string, endpoints []string, expiresAt time.Time, tunnelID, carrierConnectorID, assignmentID, configContentHash, matchType, wildcardSuffix, machineID string, installationGeneration int64, publicKey, edgeCarrierServerSPKISHA256, edgeCarrierServerCertificateChainPEM string) (AccessorAdmission, error) {
	key, err := base64.RawURLEncoding.Strict().DecodeString(publicKey)
	if err != nil {
		return AccessorAdmission{}, ErrInvalid
	}
	thumb, err := connectorprotocol.IdentityThumbprint(key)
	if err != nil {
		return AccessorAdmission{}, ErrInvalid
	}
	admission := AccessorAdmission{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessAdmissionKind, AccountID: accountID, MachineID: machineID, InstallationGeneration: uint64(installationGeneration), AccessorPublicKey: publicKey, AccessorThumbprint: thumb, ResourceKind: resourceKind, ResourceID: resourceID, TunnelName: tunnelName, RouteName: routeName, OperationID: operationID, ConnectorID: connectorID, CarrierSessionID: carrierSessionID, RouteID: routeID, RouteGeneration: uint64(routeGeneration), SessionGeneration: uint64(sessionGeneration), ProcessGeneration: uint64(processGeneration), ConfigGeneration: uint64(configGeneration), AssignmentGeneration: uint64(assignmentGeneration), AssignmentID: assignmentID, ConfigContentHash: configContentHash, EdgeNodeID: edgeNodeID, EdgeProcessEpoch: edgeEpoch, Protocol: protocol, Hostname: hostname, MatchType: matchType, WildcardSuffix: wildcardSuffix, EdgeEndpoints: append([]string(nil), endpoints...), ExpiresAt: expiresAt.UTC(), TunnelID: tunnelID, CarrierConnectorID: carrierConnectorID, EdgeCarrierServerSPKISHA256: edgeCarrierServerSPKISHA256, EdgeCarrierServerCertificateChainPEM: edgeCarrierServerCertificateChainPEM}
	if admission.Validate(now) != nil {
		return AccessorAdmission{}, ErrInvalid
	}
	return admission, nil
}

func (r *AccessorRepository) MachineRoutes(ctx context.Context, accountID, machineID string) ([]AccessorAdmission, error) {
	now := r.now().UTC()
	rows, err := r.queries.ListPrivateAccessRoutesForMachineV1(ctx, dbsqlc.ListPrivateAccessRoutesForMachineV1Params{AccountID: accountID, MachineID: machineID, Now: now})
	if err != nil {
		return nil, err
	}
	out := make([]AccessorAdmission, 0, len(rows))
	for _, v := range rows {
		if !v.AccessorPublicKey.Valid {
			return nil, ErrInvalid
		}
		a, err := admissionFromValues(now, v.AccountID, v.ResourceKind, v.ResourceID, v.TunnelName, v.RouteName, v.OperationID, v.ConnectorID, v.CarrierSessionID, v.RouteID, v.RouteGeneration, v.SessionGeneration, v.ProcessGeneration, v.ConfigGeneration, v.AssignmentGeneration, v.EdgeNodeID, v.EdgeProcessEpoch, v.Protocol, v.Hostname, v.EdgeEndpoints, v.ExpiresAt, v.TunnelID, v.CarrierConnectorID, v.AssignmentID, v.ConfigContentHash, v.MatchType, v.WildcardSuffix, v.AccessorMachineID, v.InstallationGeneration, v.AccessorPublicKey.String, v.EdgeCarrierServerSpkiSha256, v.EdgeCarrierServerCertificateChainPem)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (r *AccessorRepository) EdgeAdmissions(ctx context.Context, nodeID, epoch string) ([]AccessorAdmission, error) {
	now := r.now().UTC()
	rows, err := r.queries.ListPrivateAccessCarrierAdmissionsForEdgeV1(ctx, dbsqlc.ListPrivateAccessCarrierAdmissionsForEdgeV1Params{EdgeNodeID: nodeID, EdgeProcessEpoch: epoch, Now: now})
	if err != nil {
		return nil, err
	}
	out := make([]AccessorAdmission, 0, len(rows))
	for _, v := range rows {
		if !v.AccessorPublicKey.Valid {
			return nil, ErrInvalid
		}
		a, e := admissionFromValues(now, v.AccountID, v.ResourceKind, v.ResourceID, v.TunnelName, v.RouteName, v.OperationID, v.ConnectorID, v.CarrierSessionID, v.RouteID, v.RouteGeneration, v.SessionGeneration, v.ProcessGeneration, v.ConfigGeneration, v.AssignmentGeneration, v.EdgeNodeID, v.EdgeProcessEpoch, v.Protocol, v.Hostname, v.EdgeEndpoints, v.ExpiresAt, v.TunnelID, v.CarrierConnectorID, v.AssignmentID, v.ConfigContentHash, v.MatchType, v.WildcardSuffix, v.AccessorMachineID, v.InstallationGeneration, v.AccessorPublicKey.String, v.EdgeCarrierServerSpkiSha256, v.EdgeCarrierServerCertificateChainPem)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, nil
}

type AccessorHTTPHandler struct {
	repo interface {
		MachineRoutes(context.Context, string, string) ([]AccessorAdmission, error)
		EdgeAdmissions(context.Context, string, string) ([]AccessorAdmission, error)
	}
	machine MachineRequestVerifier
	edge    EdgeVerifier
}

func NewAccessorHTTPHandler(repo interface {
	MachineRoutes(context.Context, string, string) ([]AccessorAdmission, error)
	EdgeAdmissions(context.Context, string, string) ([]AccessorAdmission, error)
}, machine MachineRequestVerifier, edge EdgeVerifier) (*AccessorHTTPHandler, error) {
	if repo == nil || machine == nil || edge == nil {
		return nil, ErrInvalid
	}
	return &AccessorHTTPHandler{repo: repo, machine: machine, edge: edge}, nil
}

type accessorRequest struct {
	EdgeNodeID       string `json:"edge_node_id,omitempty"`
	EdgeProcessEpoch string `json:"edge_process_epoch,omitempty"`
	IdempotencyKey   string `json:"idempotency_key,omitempty"`
}

func (h *AccessorHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r == nil || r.Method != http.MethodPost {
		writeHTTPError(requestContext(r), w, http.StatusMethodNotAllowed, "method_not_allowed", "POST is required.", false)
		return
	}
	if values := r.Header.Values("Content-Type"); len(values) != 1 || values[0] != "application/json" {
		writeHTTPError(requestContext(r), w, http.StatusUnsupportedMediaType, "content_type_required", "application/json is required.", false)
		return
	}
	raw, err := readBody(r)
	if err != nil {
		writeBodyReadError(w, r, err, "authorization_unavailable", "The request is invalid.")
		return
	}
	if canonicaljson.RejectDuplicateFields(raw) != nil {
		writeHTTPError(requestContext(r), w, 400, "invalid_request", "The request is invalid.", false)
		return
	}
	var in accessorRequest
	d := json.NewDecoder(bytes.NewReader(raw))
	if d.Decode(&in) != nil {
		writeHTTPError(requestContext(r), w, 400, "invalid_request", "The request is invalid.", false)
		return
	}
	var extra any
	if !errors.Is(d.Decode(&extra), io.EOF) {
		writeHTTPError(requestContext(r), w, 400, "invalid_request", "The request is invalid.", false)
		return
	}
	var admissions []AccessorAdmission
	switch r.URL.Path {
	case MachineRoutesPath:
		idempotency, e := singleHeader(r.Header, "Idempotency-Key", false)
		if e != nil || idempotency == "" || idempotency != in.IdempotencyKey {
			capturePrivateAccessFailure(r.Context(), http.StatusBadRequest, "invalid_request", ErrInvalid, false)
			writeHTTPError(requestContext(r), w, 400, "idempotency_key_invalid", "Idempotency-Key must match the request.", false)
			return
		}
		auth, _ := singleHeader(r.Header, "Authorization", false)
		identity, _ := singleHeader(r.Header, "X-Paperboat-Machine-Identity", false)
		proofText, _ := singleHeader(r.Header, "X-Paperboat-Machine-Proof", false)
		proof, e := base64.RawURLEncoding.Strict().DecodeString(proofText)
		if e != nil || !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != identity {
			capturePrivateAccessFailure(r.Context(), http.StatusUnauthorized, "machine_identity_invalid", ErrDenied, false)
			writeHTTPError(requestContext(r), w, 401, "machine_identity_invalid", "Machine authentication is required.", false)
			return
		}
		claims, e := h.machine.VerifyMachineControlRequest(r.Context(), identity, proof, r.Method, r.URL.Path, raw)
		if e != nil {
			if clockSkewed(e) {
				writeClockSkew(requestContext(r), w, e)
				return
			}
			if pureExpectedPrivateAccessError(e, controlplane.ErrHelperProof, previewattachment.ErrUnauthorized) {
				capturePrivateAccessFailure(r.Context(), http.StatusUnauthorized, "machine_identity_invalid", e, false)
				writeHTTPError(requestContext(r), w, 401, "machine_identity_invalid", "Machine authentication is required.", false)
			} else {
				capturePrivateAccessFailure(r.Context(), http.StatusServiceUnavailable, "authorization_unavailable", e, false)
				writeHTTPError(requestContext(r), w, 503, "private_access_unavailable", "Private access discovery is unavailable.", true)
			}
			return
		}
		if claims.UserID == "" || claims.MachineID == "" || claims.OperationID != in.IdempotencyKey {
			capturePrivateAccessFailure(r.Context(), http.StatusUnauthorized, "machine_identity_invalid", ErrDenied, false)
			writeHTTPError(requestContext(r), w, 401, "machine_identity_invalid", "Machine authentication is required.", false)
			return
		}
		admissions, e = h.repo.MachineRoutes(r.Context(), claims.UserID, claims.MachineID)
		if e != nil {
			capturePrivateAccessFailure(r.Context(), http.StatusServiceUnavailable, "authorization_unavailable", e, false)
			writeHTTPError(requestContext(r), w, 503, "private_access_unavailable", "Private access discovery is unavailable.", true)
			return
		}
		for _, admission := range admissions {
			if admission.AccountID != claims.UserID || admission.MachineID != claims.MachineID || admission.InstallationGeneration != uint64(claims.InstallationGeneration) {
				capturePrivateAccessFailure(r.Context(), http.StatusUnauthorized, "machine_identity_invalid", ErrDenied, false)
				writeHTTPError(requestContext(r), w, http.StatusUnauthorized, "machine_identity_stale", "Machine authentication is stale.", false)
				return
			}
		}
	case EdgeAdmissionsPath:
		edge, e := h.edge.VerifyEdgeRequest(r.Context(), r, raw)
		if e != nil {
			if pureExpectedPrivateAccessError(e, previewattachment.ErrUnauthorized) {
				capturePrivateAccessFailure(r.Context(), http.StatusUnauthorized, "edge_identity_invalid", e, false)
				writeHTTPError(requestContext(r), w, 401, "edge_identity_invalid", "Edge authentication is required.", false)
			} else {
				capturePrivateAccessFailure(r.Context(), http.StatusServiceUnavailable, "authorization_unavailable", e, false)
				writeHTTPError(requestContext(r), w, 503, "private_access_unavailable", "Private access discovery is unavailable.", true)
			}
			return
		}
		if edge.NodeID != in.EdgeNodeID || edge.ProcessEpoch != in.EdgeProcessEpoch {
			capturePrivateAccessFailure(r.Context(), http.StatusUnauthorized, "edge_identity_invalid", ErrDenied, false)
			writeHTTPError(requestContext(r), w, 401, "edge_identity_invalid", "Edge authentication is required.", false)
			return
		}
		admissions, e = h.repo.EdgeAdmissions(r.Context(), edge.NodeID, edge.ProcessEpoch)
		if e != nil {
			capturePrivateAccessFailure(r.Context(), http.StatusServiceUnavailable, "authorization_unavailable", e, false)
			writeHTTPError(requestContext(r), w, 503, "private_access_unavailable", "Private access discovery is unavailable.", true)
			return
		}
	default:
		writeHTTPError(requestContext(r), w, 404, "not_found", "Not found.", false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(AccessorSnapshot{Schema: "paperboat.preview-tunnel/v1", Kind: "private_access_carrier_snapshot", Complete: true, Admissions: admissions}); err != nil {
		capturePrivateAccessFailure(r.Context(), http.StatusOK, "authorization_unavailable", err, false)
	}
}

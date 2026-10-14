package httpapi

import (
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/operatorinfra"
)

type operatorRegionInput struct {
	Revision    int64  `json:"revision"`
	DisplayName string `json:"display_name,omitempty"`
}
type operatorNodeInput struct {
	RegionID               string   `json:"region_id,omitempty"`
	EdgePool               string   `json:"edge_pool,omitempty"`
	FailureDomain          string   `json:"failure_domain,omitempty"`
	EndpointHost           string   `json:"endpoint_host,omitempty"`
	EndpointTCPPort        int32    `json:"endpoint_tcp_port,omitempty"`
	EndpointQUICPort       int32    `json:"endpoint_quic_port,omitempty"`
	Roles                  []string `json:"roles,omitempty"`
	Transports             []string `json:"transports,omitempty"`
	CapacityLimit          int64    `json:"capacity_limit,omitempty"`
	Revision               int64    `json:"revision"`
	UsagePublicKey         []byte   `json:"usage_public_key,omitempty"`
	PeerWireGuardPublicKey []byte   `json:"peer_wireguard_public_key,omitempty"`
	PeerDiscoPublicKey     []byte   `json:"peer_disco_public_key,omitempty"`
	PeerVirtualAddress     string   `json:"peer_virtual_address,omitempty"`
}

func registerOperatorInfrastructure(mux *http.ServeMux, opts Options) {
	service := opts.OperatorInfrastructure
	if service == nil {
		return
	}
	for _, kind := range []string{"regions", "nodes"} {
		mux.Handle("GET /v1/admin/"+kind, operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit, ok := operatorPageLimit(w, r, 100)
			if !ok {
				return
			}
			p, _ := principalFromContext(r.Context())
			var out any
			var err error
			if kind == "regions" {
				out, err = service.ListRegions(r.Context(), p.User.ID, r.URL.Query().Get("after"), limit)
			} else {
				out, err = service.ListNodes(r.Context(), p.User.ID, r.URL.Query().Get("region"), r.URL.Query().Get("after"), limit)
			}
			if err != nil {
				operatorInfrastructureError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
	}
	for _, action := range []string{"create", "update", "enable", "disable"} {
		mux.Handle("POST /v1/admin/regions/{region_id}/"+action, operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in operatorRegionInput
			r.Body = http.MaxBytesReader(w, r.Body, 8192)
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			if (action == "enable" || action == "disable") && in.DisplayName != "" {
				writeError(w, r, 400, "invalid_request", "Only revision is accepted for region enable/disable.")
				return
			}
			p, _ := principalFromContext(r.Context())
			out, err := service.MutateRegion(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"), operatorinfra.RegionMutation{Action: action, ID: r.PathValue("region_id"), DisplayName: in.DisplayName, Reason: r.Header.Get("Operator-Reason"), Revision: in.Revision})
			if err != nil {
				operatorInfrastructureError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
	}
	for _, action := range []string{"enroll", "update", "drain", "resume", "retire", "rotate", "verify-ingress"} {
		mux.Handle("POST /v1/admin/nodes/{node_id}/"+action, operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in operatorNodeInput
			r.Body = http.MaxBytesReader(w, r.Body, 8192)
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			if action != "enroll" && action != "update" && (in.RegionID != "" || in.EdgePool != "" || in.FailureDomain != "" || in.EndpointHost != "" || in.EndpointTCPPort != 0 || in.EndpointQUICPort != 0 || len(in.Roles) != 0 || len(in.Transports) != 0 || in.CapacityLimit != 0 || len(in.UsagePublicKey) != 0 || len(in.PeerWireGuardPublicKey) != 0 || len(in.PeerDiscoPublicKey) != 0 || in.PeerVirtualAddress != "") {
				writeError(w, r, 400, "invalid_request", "Only revision is accepted for this node action.")
				return
			}
			p, _ := principalFromContext(r.Context())
			out, err := service.MutateNode(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"), operatorinfra.NodeMutation{Action: action, NodeID: r.PathValue("node_id"), RegionID: in.RegionID, EdgePool: in.EdgePool, FailureDomain: in.FailureDomain, EndpointHost: in.EndpointHost, EndpointTCPPort: in.EndpointTCPPort, EndpointQUICPort: in.EndpointQUICPort, Roles: in.Roles, Transports: in.Transports, CapacityLimit: in.CapacityLimit, Revision: in.Revision, UsagePublicKey: in.UsagePublicKey, PeerWireGuardPublicKey: in.PeerWireGuardPublicKey, PeerDiscoPublicKey: in.PeerDiscoPublicKey, PeerVirtualAddress: in.PeerVirtualAddress, Reason: r.Header.Get("Operator-Reason")})
			if err != nil {
				operatorInfrastructureError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
	}
}
func operatorInfrastructureError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case expectedHTTPErrorOnly(err, operatorinfra.ErrInvalid):
		writeError(w, r, 400, "invalid_request", "Use a valid region/node configuration and explicit revision.")
	case expectedHTTPErrorOnly(err, operatorinfra.ErrForbidden):
		writeError(w, r, 403, "forbidden", "Current staff authority does not permit this infrastructure action.")
	case expectedHTTPErrorOnly(err, operatorinfra.ErrNotFound):
		writeError(w, r, 404, "not_found", "The managed infrastructure target does not exist.")
	case expectedHTTPErrorOnly(err, operatorinfra.ErrConflict):
		writeError(w, r, 409, "infrastructure_conflict", "The target state, revision or retry key conflicts. Read current state before changing the request; retry interrupted requests with their original key.")
	default:
		writeError(w, r, 503, "infrastructure_unavailable", "Infrastructure authority is unavailable. Retry with the same request and key.", err)
	}
}

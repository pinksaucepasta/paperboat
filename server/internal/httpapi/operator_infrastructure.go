package httpapi

import (
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/operatorinfra"
)

type operatorRegionInput struct {
	Revision    int64  `json:"revision"`
	DisplayName string `json:"display_name,omitempty"`
}
type operatorNodeInput struct {
	Revision int64  `json:"revision"`
	Site     string `json:"site,omitempty"`
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
	mux.Handle("GET /v1/admin/publications", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit, ok := operatorPageLimit(w, r, 100)
		if !ok {
			return
		}
		p, _ := principalFromContext(r.Context())
		out, err := service.ListPublicationBlocks(r.Context(), p.User.ID, r.URL.Query().Get("blocked"), r.URL.Query().Get("after"), limit)
		if err != nil {
			operatorInfrastructureError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: out})
	})))
	for _, action := range []string{"create", "update", "enable", "disable", "replace"} {
		mux.Handle("POST /v1/admin/regions/{region_id}/"+action, operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in operatorRegionInput
			r.Body = http.MaxBytesReader(w, r.Body, 8192)
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			if (action == "enable" || action == "disable" || action == "replace") && in.DisplayName != "" {
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
	mux.Handle("DELETE /v1/admin/regions/{region_id}", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFromContext(r.Context())
		out, err := service.DeleteRegion(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"), r.PathValue("region_id"), r.Header.Get("Operator-Reason"))
		if err != nil {
			operatorInfrastructureError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: out})
	})))
	for _, action := range []string{"drain", "drain-move", "resume", "retire", "verify-ingress", "site"} {
		mux.Handle("POST /v1/admin/nodes/{node_id}/"+action, operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in operatorNodeInput
			r.Body = http.MaxBytesReader(w, r.Body, 8192)
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			if action == "site" && in.Revision != 0 {
				writeError(w, r, 400, "invalid_request", "Only site is accepted for this node action.")
				return
			}
			if action != "site" && in.Site != "" {
				writeError(w, r, 400, "invalid_request", "Only revision is accepted for this node action.")
				return
			}
			p, _ := principalFromContext(r.Context())
			out, err := service.MutateNode(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"), operatorinfra.NodeMutation{Action: action, NodeID: r.PathValue("node_id"), Revision: in.Revision, Site: in.Site, Reason: r.Header.Get("Operator-Reason")})
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
	case expectedHTTPErrorOnly(err, operatorinfra.ErrLastServingEdge):
		writeError(w, r, 409, "last_serving_edge", "This is the last serving edge of an enabled region. Bring another edge in the region up first, or disable the region.")
	case expectedHTTPErrorOnly(err, operatorinfra.ErrRegionInUse):
		writeError(w, r, 409, "region_in_use", "This region is still in use. Retire its nodes, re-place its workspaces, and remove any self-hosted region using this ID before retrying.")
	case expectedHTTPErrorOnly(err, operatorinfra.ErrConflict):
		writeError(w, r, 409, "infrastructure_conflict", "The target state, revision or retry key conflicts. Read current state before changing the request; retry interrupted requests with their original key.")
	default:
		writeError(w, r, 503, "infrastructure_unavailable", "Infrastructure authority is unavailable. Retry with the same request and key.", err)
	}
}

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/servernodes"
)

// registerServerNodes is the operator panel's server-node API; any enrolled node serves it (equal peers).
func registerServerNodes(mux *http.ServeMux, opts Options) {
	enroller := opts.ServerNodes
	if enroller == nil {
		return
	}
	mux.Handle("GET /v1/admin/server-nodes", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodes, err := servernodes.List(r.Context(), enroller.DB)
		if err != nil {
			writeError(w, r, 503, "server_nodes_unavailable", "Server nodes could not be read. Retry shortly.", err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"nodes": nodes}})
	})))
	mux.Handle("POST /v1/admin/server-nodes/enrollments", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Address string `json:"address"`
			Code    string `json:"code"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		p, _ := principalFromContext(r.Context())
		node, err := enroller.Enroll(r.Context(), p.User.ID, in.Address, in.Code)
		if err != nil {
			serverNodeError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: node})
	})))
	for action, apply := range map[string]func(context.Context, *db.DB, string) (servernodes.Status, error){"drain": servernodes.Drain, "remove": servernodes.Remove} {
		mux.Handle("POST /v1/admin/server-nodes/{node_id}/"+action, operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, 1024)
			var in struct{}
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			node, err := apply(r.Context(), enroller.DB, r.PathValue("node_id"))
			if err != nil {
				serverNodeError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: node})
		})))
	}
}

func serverNodeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, servernodes.ErrInvalid):
		writeError(w, r, 400, "invalid_request", "Use the node's address and the claim code it printed.")
	case errors.Is(err, servernodes.ErrUnauthorized):
		writeError(w, r, 403, "claim_code_mismatch", "The node at this address did not prove this claim code. Check the address and copy the code again.")
	case errors.Is(err, servernodes.ErrExpired):
		writeError(w, r, 410, "claim_code_expired", "The claim code expired. Run enroll-node on the new node again for a fresh code.")
	case errors.Is(err, servernodes.ErrNotFound):
		writeError(w, r, 404, "server_node_not_found", "No server node has this id. Refresh the list.")
	case errors.Is(err, servernodes.ErrState):
		writeError(w, r, 409, "server_node_state_conflict", "The node is not in a state this action applies to (a removed node cannot be drained or removed again). Refresh the list.")
	case errors.Is(err, servernodes.ErrUsed):
		writeError(w, r, 409, "server_node_exists", "That code was already used, or a node with this region and instance is enrolled. Pick a free instance slot.")
	default:
		writeError(w, r, 502, "server_node_unreachable", "The new node could not be reached at this address. Check that enroll-node is running and port 8443 is open, then retry.", err)
	}
}

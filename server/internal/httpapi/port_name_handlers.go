package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
)

func portNameError(w http.ResponseWriter, r *http.Request, e error) {
	switch {
	case expectedHTTPErrorOnly(e, machineservices.ErrNameIsMachine):
		writeError(w, r, 409, "name_is_machine", "A machine already has this name, so the address opens that machine. Choose another name.")
	case expectedHTTPErrorOnly(e, machineservices.ErrPortNameNotFound):
		writeError(w, r, 404, "not_found", "No such port name in this workspace.")
	case expectedHTTPErrorOnly(e, wire.ErrInvalid):
		writeError(w, r, 400, "invalid_request", "A name is one lowercase DNS label that is not a number (letters, digits and '-', at most 63), and a port is 1-65535.")
	case expectedHTTPErrorOnly(e, machineservices.ErrDenied):
		writeError(w, r, 403, "forbidden", "Machine names need machine management; all-machines and top-level names need a workspace owner or admin.")
	default:
		machineServiceError(w, r, e)
	}
}

// portNameWorkspace is the selected workspace, personal when none is selected.
func portNameWorkspace(r *http.Request) workspace.Scope {
	if scope, ok := workspace.FromContext(r.Context()); ok {
		return scope
	}
	return workspace.Scope{ID: "personal"}
}

func decodePortNameBody(r *http.Request, into any) bool {
	d := json.NewDecoder(io.LimitReader(r.Body, 4097))
	return d.Decode(into) == nil && d.Decode(&struct{}{}) == io.EOF
}

// portNames serves GET (list), POST (set) and DELETE /{name_id} of the selected workspace's port names.
func portNames(s *machineservices.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		scope := portNameWorkspace(r)
		switch r.Method {
		case http.MethodPost:
			var in wire.PortNameSet
			if !decodePortNameBody(r, &in) {
				portNameError(w, r, wire.ErrInvalid)
				return
			}
			record, e := s.SetPortName(r.Context(), p.User.ID, scope, in)
			if e != nil {
				portNameError(w, r, e)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: record})
		case http.MethodDelete:
			if e := s.DeletePortName(r.Context(), p.User.ID, scope, r.PathValue("name_id")); e != nil {
				portNameError(w, r, e)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			items, e := s.ListPortNames(r.Context(), p.User.ID, scope)
			if e != nil {
				portNameError(w, r, e)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: map[string]any{"items": items}})
		}
	}
}

// machineFrontDesk serves GET and PUT /v1/machines/{machine_id}/front-desk.
func machineFrontDesk(s *machineservices.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		machine := r.PathValue("machine_id")
		if r.Method == http.MethodPut {
			var in wire.FrontDesk
			if !decodePortNameBody(r, &in) {
				portNameError(w, r, wire.ErrInvalid)
				return
			}
			if e := s.SetFrontDesk(r.Context(), p.User.ID, machine, in); e != nil {
				portNameError(w, r, e)
				return
			}
		}
		desk, e := s.GetFrontDesk(r.Context(), p.User.ID, machine)
		if e != nil {
			portNameError(w, r, e)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: desk})
	}
}

package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

type SelfhostHandlers struct{ Service *selfhost.Service }

func selfhostJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func selfhostError(w http.ResponseWriter, r *http.Request, e error) {
	status, code, message := 503, "selfhost_unavailable", "Server enrollment is unavailable; retry shortly."
	switch {
	case expectedHTTPErrorOnly(e, selfhost.ErrInvalid):
		status, code, message = 400, "invalid_request", e.Error()
	case expectedHTTPErrorOnly(e, selfhost.ErrInstallationNotFound):
		status, code, message = 404, "installation_not_found", "Installation was not found."
	case expectedHTTPErrorOnly(e, selfhost.ErrUnauthorized):
		status, code, message = 403, "installation_unauthorized", e.Error()
	case expectedHTTPErrorOnly(e, selfhost.ErrExpired):
		status, code, message = 410, "enrollment_expired", e.Error()
	case expectedHTTPErrorOnly(e, selfhost.ErrUsed):
		status, code, message = 409, "enrollment_used", e.Error()
	case expectedHTTPErrorOnly(e, selfhost.ErrRevoked):
		status, code, message = 410, "installation_revoked", e.Error()
	case expectedHTTPErrorOnly(e, selfhost.ErrUnreachable):
		status, code, message = 502, "server_unreachable", e.Error()
	case expectedHTTPErrorOnly(e, selfhost.ErrIngress) || selfhost.IsIngressProbeFailure(e):
		status, code, message = 409, "ingress_unverified", e.Error()
	case expectedHTTPErrorOnly(e, selfhost.ErrLimit):
		status, code, message = 429, "selfhost_limit", e.Error()
	case expectedHTTPErrorOnly(e, selfhost.ErrIncompatibleTopology):
		status, code, message = 409, "selfhost_topology_incompatible", e.Error()
	}
	if status >= http.StatusInternalServerError {
		w.Header().Set("Cache-Control", "no-store")
		writeError(w, r, status, code, message, e)
		return
	}
	logHTTPRejection(r, status, code, e)
	selfhostJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}
func selfhostRead(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return nil, selfhost.ErrInvalid
	}
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if e != nil {
		return nil, selfhost.ErrInvalid
	}
	return b, nil
}
func selfhostDecode(body []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(&struct{}{}) != io.EOF {
		return selfhost.ErrInvalid
	}
	return nil
}
func (h *SelfhostHandlers) Account(w http.ResponseWriter, r *http.Request) { h.handle(w, r, false) }
func (h *SelfhostHandlers) Global(w http.ResponseWriter, r *http.Request)  { h.handle(w, r, true) }
func (h *SelfhostHandlers) handle(w http.ResponseWriter, r *http.Request, operator bool) {
	if h == nil || h.Service == nil {
		selfhostError(w, r, errors.New("unavailable"))
		return
	}
	principal, ok := principalFromContext(r.Context())
	if !ok || principal.User.ID == "" {
		selfhostJSON(w, 401, map[string]any{"error": map[string]string{"code": "unauthenticated", "message": "Sign into Paperboat."}})
		return
	}
	actor := principal.User.ID
	q := r.URL.Query()
	if r.Method == "GET" && r.PathValue("installationID") != "" && len(q) != 0 {
		selfhostError(w, r, selfhost.ErrInvalid)
		return
	}
	isInventory := r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/installations")
	for key, values := range q {
		inventoryKey := key == "limit" || key == "offset" || key == "q" || key == "capability" || key == "enrollment_state" || key == "ready" || key == "sort"
		if (key != "scope_kind" && key != "scope_id" && !(isInventory && inventoryKey)) || len(values) != 1 {
			selfhostError(w, r, selfhost.ErrInvalid)
			return
		}
	}
	scope := selfhost.Scope{Kind: q.Get("scope_kind"), ID: q.Get("scope_id")}
	if operator {
		scope = selfhost.Scope{Kind: "global", ID: "global"}
	}
	var out any = map[string]any{}
	var e error
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/enrollments"):
		var b []byte
		b, e = selfhostRead(w, r)
		var in selfhost.EnrollRequest
		if e == nil {
			e = selfhostDecode(b, &in)
		}
		if operator {
			in.Scope = scope
		}
		if e == nil {
			var list []selfhost.Installation
			list, e = h.Service.Enroll(r.Context(), actor, in, operator)
			out = map[string]any{"installations": list}
		}
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/installations"):
		var options selfhost.InventoryOptions
		options, e = selfhostInventoryOptions(r)
		if e == nil {
			out, e = h.Service.ListScopedInventory(r.Context(), actor, scope, operator, options)
		}
	case r.Method == "GET" && r.PathValue("installationID") != "":
		out, e = h.Service.GetInstallation(r.Context(), actor, r.PathValue("installationID"), operator)
	case r.PathValue("resourceKind") != "":
		kind, id := r.PathValue("resourceKind"), r.PathValue("resourceID")
		if r.Method == "GET" {
			out, e = h.Service.Placement(r.Context(), actor, kind, id)
		} else {
			var b []byte
			b, e = selfhostRead(w, r)
			var in struct {
				InstallationID *string `json:"installation_id"`
			}
			if e == nil {
				e = selfhostDecode(b, &in)
			}
			if e == nil {
				out, e = h.Service.SetPlacement(r.Context(), actor, kind, id, in.InstallationID)
			}
		}
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/verify-ingress"):
		e = h.Service.VerifyIngressScoped(r.Context(), actor, r.PathValue("installationID"), operator)
	case r.Method == "DELETE" && r.PathValue("installationID") != "":
		e = h.Service.RemoveScoped(r.Context(), actor, r.PathValue("installationID"), operator)
	case r.Method == "GET" && r.PathValue("capability") != "":
		out, e = h.Service.PoolScoped(r.Context(), actor, r.PathValue("capability"), scope)
	case r.Method == "PUT" && r.PathValue("capability") != "":
		var b []byte
		b, e = selfhostRead(w, r)
		var p selfhost.Policy
		if e == nil {
			e = selfhostDecode(b, &p)
		}
		if e == nil {
			out, e = h.Service.SetPoolScoped(r.Context(), actor, r.PathValue("capability"), p, scope)
		}
	default:
		e = selfhost.ErrInvalid
	}
	if e != nil {
		selfhostError(w, r, e)
		return
	}
	selfhostJSON(w, 200, map[string]any{"data": out})
}

func selfhostInventoryOptions(r *http.Request) (selfhost.InventoryOptions, error) {
	q := r.URL.Query()
	options := selfhost.InventoryOptions{Limit: 50, Q: q.Get("q"), Capability: q.Get("capability"), EnrollmentState: q.Get("enrollment_state")}
	for key, values := range q {
		switch key {
		case "scope_kind", "scope_id", "limit", "offset", "q", "capability", "enrollment_state", "ready", "sort":
		default:
			return options, selfhost.ErrInvalid
		}
		if len(values) != 1 {
			return options, selfhost.ErrInvalid
		}
	}
	switch q.Get("sort") {
	case "", "name":
	case "newest":
		options.Newest = true
	default:
		return options, selfhost.ErrInvalid
	}
	if kind := q.Get("scope_kind"); kind != "" && kind != "account" && kind != "team" && kind != "global" {
		return options, selfhost.ErrInvalid
	}
	if id := q.Get("scope_id"); len(id) > 128 || !utf8.ValidString(id) || strings.ContainsRune(id, '\x00') || (id != "" && q.Get("scope_kind") == "") {
		return options, selfhost.ErrInvalid
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > 200 {
			return options, selfhost.ErrInvalid
		}
		options.Limit = n
	}
	if q.Has("offset") {
		n, err := strconv.Atoi(q.Get("offset"))
		if err != nil || n < 0 || n > 2147483647 {
			return options, selfhost.ErrInvalid
		}
		options.Offset = n
	}
	if q.Has("ready") {
		switch q.Get("ready") {
		case "true":
			v := true
			options.Ready = &v
		case "false":
			v := false
			options.Ready = &v
		default:
			return options, selfhost.ErrInvalid
		}
	}
	if options.Capability != "" && options.Capability != "relay" && options.Capability != "tunnel" {
		return options, selfhost.ErrInvalid
	}
	if options.EnrollmentState != "" && options.EnrollmentState != "pending" && options.EnrollmentState != "enrolled" {
		return options, selfhost.ErrInvalid
	}
	if !utf8.ValidString(options.Q) || len(options.Q) > 160 || strings.ContainsRune(options.Q, '\x00') {
		return options, selfhost.ErrInvalid
	}
	return options, nil
}

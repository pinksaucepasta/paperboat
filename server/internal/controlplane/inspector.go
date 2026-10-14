package controlplane

import (
	"github.com/pinksaucepasta/paperboat-server/internal/inspectaccess"
	"net/http"
)

func (s *EdgeService) handleInspectorAccess(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in inspectaccess.AccessRequest
	if !s.decode(w, r, &in) {
		return
	}
	if in.EdgeNodeID == "" || in.EdgeProcessEpoch == "" || r.Header.Get("X-Paperboat-Edge-Node-ID") != in.EdgeNodeID || r.Header.Get("X-Paperboat-Edge-Process-Epoch") != in.EdgeProcessEpoch || in.Transport != "" {
		writeEdgeError(w, r, http.StatusForbidden, "inspector_not_authorized", false, 0, inspectaccess.ErrNotAuthorized)
		return
	}
	in.Transport = "edge"
	out, err := inspectaccess.NewService(s.store).ResolveAccess(r.Context(), in)
	if err != nil {
		if inspectaccess.IsExactExpectedRejection(err) {
			writeEdgeError(w, r, http.StatusForbidden, "inspector_not_authorized", false, 0, err)
		} else {
			writeEdgeError(w, r, http.StatusServiceUnavailable, "inspector_unavailable", true, 1000, err)
		}
		return
	}
	writeEdgeJSON(w, http.StatusOK, out)
}

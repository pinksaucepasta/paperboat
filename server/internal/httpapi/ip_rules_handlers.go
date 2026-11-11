package httpapi

import (
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/accessrules"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
)

// ipRules serves GET and PUT of one tunnel's or preview's owner IP lists (edge abuse plan 3.5). kind is
// "tunnel" or "preview"; param names the path value holding its id.
func ipRules(store *accessrules.Store, kind, param string, write bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		request, ok := previewTunnelRequestContext(w, r)
		if !ok {
			return
		}
		action := "read"
		if write {
			action = "write"
		}
		if err := previewtunnelapi.Authorize(request.Actor, previewtunnelapi.AccessRequest{AccountID: request.Actor.AccountID, Resource: kind + "s", Action: action}); err != nil {
			writePreviewTunnelError(w, r, http.StatusForbidden, "forbidden", "This credential cannot "+action+" "+kind+"s.", "unchanged", false, "use_authorized_credential")
			return
		}
		var rules accessrules.Rules
		var err error
		if write {
			if !decodeStrictJSON(w, r, &rules) {
				return
			}
			rules, err = store.Put(r.Context(), request.Actor.ActorID, kind, r.PathValue(param), rules)
		} else {
			rules, err = store.Get(r.Context(), request.Actor.ActorID, kind, r.PathValue(param))
		}
		switch {
		case errors.Is(err, accessrules.ErrInvalid):
			writePreviewTunnelError(w, r, http.StatusBadRequest, "invalid_ip_rules", err.Error(), "unchanged", false, "fix_request")
		case errors.Is(err, accessrules.ErrNotFound):
			writePreviewTunnelError(w, r, http.StatusNotFound, kind+"_not_found", "The "+kind+" was not found, or this account cannot manage it.", "unchanged", false, "refresh")
		case err != nil:
			writePreviewTunnelError(w, r, http.StatusServiceUnavailable, "ip_rules_unavailable", "IP rules are unavailable. Try again.", "unknown", true, "retry", err)
		default:
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, SuccessResponse{Data: rules})
		}
	}
}

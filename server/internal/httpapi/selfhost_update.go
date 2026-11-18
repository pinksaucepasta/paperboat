package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/clientupdate"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

// UpdateCheck is pbh's check-in (its node's runtime credential): it reports its own version and learns the update
// an operator requested, if any. A pbh below its update floor is told so (426) with how to update.
func (h *SelfhostHandlers) UpdateCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	credential, ok := bearerToken(r)
	if !ok || len(r.Header.Values("Authorization")) != 1 {
		writeError(w, r, 401, "runtime_unauthorized", "Supply this node's runtime credential.", nil)
		return
	}
	var in struct {
		Version string `json:"version"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in) != nil {
		writeError(w, r, 400, "invalid_request", `Send {"version": <this pbh release>}.`, nil)
		return
	}
	request, err := h.Service.UpdateCheck(r.Context(), credential)
	switch {
	case errors.Is(err, selfhost.ErrUnauthorized):
		writeError(w, r, 401, "runtime_unauthorized", "This node's credential is no longer accepted; contact its operator.", nil)
		return
	case err != nil:
		writeError(w, r, 503, "update_check_unavailable", "The update check is unavailable; retry shortly.", err)
		return
	}
	// An operator's update request comes first: it is how a node below the floor gets current.
	if request == nil {
		if floor := h.Floors["pbh"]; floor != nil {
			if _, release := clientupdate.ParseVersion(in.Version); release {
				if policy, err := floor.Get(r.Context()); err == nil && clientupdate.RequiresUpdate(policy, in.Version, time.Now()) {
					writeErrorDetails(w, r, http.StatusUpgradeRequired, "update_required", controlplane.UpdateRequiredMessage("pbh", in.Version, policy), map[string]any{"minimum_version": policy.MinimumVersion, "reason": policy.Reason})
					return
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"update": request}})
}

// UpdateReport records one step of the node's running update, with the exact failure and its support reference.
func (h *SelfhostHandlers) UpdateReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	credential, ok := bearerToken(r)
	if !ok || len(r.Header.Values("Authorization")) != 1 {
		writeError(w, r, 401, "runtime_unauthorized", "Supply this node's runtime credential.", nil)
		return
	}
	var in struct {
		ID               string `json:"id"`
		State            string `json:"state"`
		Detail           string `json:"detail"`
		SupportReference string `json:"support_reference"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in) != nil {
		writeError(w, r, 400, "invalid_request", "Send the update id, its state, and an optional detail and support reference.", nil)
		return
	}
	err := h.Service.UpdateReport(r.Context(), credential, in.ID, in.State, in.Detail, in.SupportReference)
	switch {
	case errors.Is(err, selfhost.ErrUnauthorized):
		writeError(w, r, 401, "runtime_unauthorized", "This node's credential is no longer accepted; contact its operator.", nil)
	case errors.Is(err, selfhost.ErrInvalid):
		writeError(w, r, 400, "invalid_request", "Use a known step, a detail of at most 1024 bytes and a support reference of at most 128.", nil)
	case errors.Is(err, selfhost.ErrUpdateNotRunning):
		writeError(w, r, 409, "update_not_running", "This update is not running on this node (finished, replaced or another node's).", nil)
	case err != nil:
		writeError(w, r, 503, "update_report_unavailable", "The update report is unavailable; retry shortly.", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

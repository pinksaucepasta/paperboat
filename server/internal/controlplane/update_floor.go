package controlplane

import (
	"context"
	"fmt"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/clientupdate"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/shared/nodemetrics"
)

// NodeUpdateFloor answers one node component's update floor (clientupdate.NewNode).
type NodeUpdateFloor interface {
	Get(context.Context) (clientupdate.Policy, error)
}

// SetUpdateFloors sets the floor per node component ("edge", "relay").
func (s *EdgeService) SetUpdateFloors(floors map[string]NodeUpdateFloor) { s.updateFloors = floors }

// UpdateRequiredMessage is the one text a node below its floor is told, and that console and admin show.
func UpdateRequiredMessage(component, version string, p clientupdate.Policy) string {
	return fmt.Sprintf("Update required: Paperboat %s %s is older than the minimum supported version %s (%s). "+
		"Run the pbh install command on the host again to update it.",
		component, version, p.MinimumVersion, p.Reason)
}

// refuseBelowFloor answers 426 update_required when a node reports a release version below its component's
// floor. Development builds (no release version) and a floor that cannot be read are let through: a database
// hiccup must never take nodes offline.
func (s *EdgeService) refuseBelowFloor(w http.ResponseWriter, r *http.Request, component string, metrics *nodemetrics.Report) bool {
	floor := s.updateFloors[component]
	if floor == nil || metrics == nil {
		return false
	}
	if _, ok := clientupdate.ParseVersion(metrics.Version); !ok {
		return false
	}
	policy, err := floor.Get(r.Context())
	if err != nil || !clientupdate.RequiresUpdate(policy, metrics.Version, s.clock()) {
		return false
	}
	warnEdgeRejection(r, "invalid_request", http.StatusUpgradeRequired, nil)
	requestID := serverdiagnostics.RequestID(r.Context())
	if requestID == "" {
		requestID = "req_unknown"
	}
	writeEdgeJSON(w, http.StatusUpgradeRequired, map[string]any{
		"code": "update_required", "message": UpdateRequiredMessage(component, metrics.Version, policy), "requestId": requestID,
		"retryable": false, "minimum_version": policy.MinimumVersion, "reason": policy.Reason,
	})
	return true
}

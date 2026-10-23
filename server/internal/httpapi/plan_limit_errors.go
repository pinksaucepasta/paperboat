package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
)

// writePlanLimitReached answers a new resource refused past its plan's count limit with the
// typed code (machine_limit_reached, preview_limit_reached, browser_terminal_limit_reached),
// the limit, the plan and the console Billing page to upgrade from.
func writePlanLimitReached(w http.ResponseWriter, r *http.Request, err error) bool {
	var limit *planlimits.LimitError
	if !errors.As(err, &limit) {
		return false
	}
	details := map[string]any{"kind": string(limit.Kind), "plan": string(limit.Plan), "limit": limit.Limit, "used": limit.Used}
	if limit.BillingURL != "" {
		details["billing_url"] = limit.BillingURL
	}
	writeErrorDetails(w, r, http.StatusConflict, limit.Code(), planLimitMessage(limit), details)
	return true
}

func planLimitMessage(l *planlimits.LimitError) string {
	switch l.Kind {
	case planlimits.Machines:
		return fmt.Sprintf("This workspace has reached its plan's limit of %d machines. Remove machines you no longer use, or upgrade your plan.", l.Limit)
	case planlimits.Publications:
		return fmt.Sprintf("This workspace has reached its plan's limit of %d active previews and tunnels. Stop one you no longer need, or upgrade your plan.", l.Limit)
	default:
		return fmt.Sprintf("This workspace has reached its plan's limit of %d open browser terminals. Close one, or upgrade your plan.", l.Limit)
	}
}

// writePreviewTunnelLimitReached answers a new preview or tunnel past the plan's active limit in
// the preview/tunnel error schema: nothing was created, upgrading or stopping one is the way out.
func writePreviewTunnelLimitReached(w http.ResponseWriter, r *http.Request, err error) bool {
	var limit *planlimits.LimitError
	if !errors.As(err, &limit) {
		return false
	}
	writePreviewTunnelError(w, r, http.StatusConflict, limit.Code(), planLimitMessage(limit)+" No resource was created.", "unchanged", false, "upgrade_plan")
	return true
}

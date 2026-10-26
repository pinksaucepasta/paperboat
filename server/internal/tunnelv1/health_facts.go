package tunnelv1

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// HealthFacts are the live observations a tunnel's status explanation is built from. They are read
// when status is requested so a stopped host, an unready edge, a domain still waiting for DNS or an
// exhausted bandwidth allowance is named instead of everything collapsing into "pending".
type HealthFacts struct {
	// ActiveConnectors counts the tunnel's connectors that are neither revoked nor deleted.
	ActiveConnectors int
	// ConnectorSessions counts those connectors with a ready session whose heartbeat is current.
	ConnectorSessions int
	// EdgeReady reports whether any edge node can currently accept tunnel traffic.
	EdgeReady bool
	// FailedRoutes counts active routes whose edge assignment reported failed or degraded.
	FailedRoutes int
	// PendingDomains and PendingCertificates hold hostnames still waiting for DNS ownership
	// verification, or for their certificate once ownership is verified.
	PendingDomains      []string
	PendingCertificates []string
	// OriginReason is why the local app is unreachable (refused, timeout, unreachable, tls), or "".
	OriginReason string
	// BandwidthExhausted reports that the workspace has used its bandwidth allowance.
	BandwidthExhausted bool
	// EngineServed reports that the endpoint engine serves the tunnel (PLAN 3a step 4): an active
	// tunnel endpoint with an edge assignment observed ready. Connector facts then say nothing about
	// whether it is served.
	EngineServed bool
}

// healthFactsFreshness is how recent a heartbeat or edge observation must be to count as live.
const healthFactsFreshness = 15 * time.Second

// HealthFacts reads the live observations for one tunnel.
func (r *SQLRepository) HealthFacts(ctx context.Context, accountID, tunnelID string, now time.Time) (HealthFacts, error) {
	var facts HealthFacts
	fresh := now.Add(-healthFactsFreshness)
	err := r.db.Pool().QueryRow(ctx, `SELECT
 (SELECT count(*) FROM tunnel_connectors c WHERE c.tunnel_id=$2 AND c.desired_state='active' AND c.revoked_at IS NULL),
 (SELECT count(*) FROM tunnel_connectors c JOIN tunnel_connector_sessions s ON s.id=c.last_session_id AND s.connector_id=c.id
   WHERE c.tunnel_id=$2 AND c.desired_state='active' AND c.revoked_at IS NULL AND s.state='ready' AND s.lease_deadline>$3 AND s.last_heartbeat_at>$4),
 EXISTS(SELECT 1 FROM control_tunnel_nodes n WHERE n.state='ready' AND n.ready AND n.roles @> ARRAY['edge']::text[]
   AND n.registry_expires_at>$3 AND n.last_heartbeat_at>$4 AND n.drain_deadline IS NULL),
 (SELECT count(*) FROM tunnel_edge_route_assignments a JOIN tunnel_routes r ON r.id=a.route_id AND r.desired_state='active' AND r.deleted_at IS NULL
   WHERE a.account_id=$1 AND a.tunnel_id=$2 AND a.state='active' AND a.observed_state IN ('failed','degraded') AND a.observed_at>$3::timestamptz-interval '1 minute'),
 coalesce((SELECT oh.code FROM tunnel_route_origin_health oh JOIN tunnel_routes r ON r.id=oh.route_id AND r.desired_state='active' AND r.deleted_at IS NULL
   WHERE oh.tunnel_id=$2 AND oh.state='unreachable' AND oh.observed_at>$3::timestamptz-($5::double precision*interval '1 second') ORDER BY oh.observed_at DESC LIMIT 1),''),
 EXISTS(SELECT 1 FROM endpoints e JOIN endpoint_edge_assignments x ON x.endpoint_id=e.id
   WHERE e.tunnel_id=$2 AND e.kind='tunnel' AND e.desired_state='active' AND x.desired='active' AND x.observed_ready)`,
		accountID, tunnelID, now, fresh, OriginHealthMaxAge.Seconds()).
		Scan(&facts.ActiveConnectors, &facts.ConnectorSessions, &facts.EdgeReady, &facts.FailedRoutes, &facts.OriginReason, &facts.EngineServed)
	if err != nil {
		return HealthFacts{}, translate(err)
	}
	rows, err := r.db.Pool().Query(ctx, `SELECT hostname, ownership_state, certificate_state, certificate_strategy FROM tunnel_domains
 WHERE account_id=$1 AND tunnel_id=$2 AND deleted_at IS NULL ORDER BY hostname LIMIT 64`, accountID, tunnelID)
	if err != nil {
		return HealthFacts{}, translate(err)
	}
	defer rows.Close()
	for rows.Next() {
		var hostname, ownership, certificate, strategy string
		if err := rows.Scan(&hostname, &ownership, &certificate, &strategy); err != nil {
			return HealthFacts{}, translate(err)
		}
		switch {
		case ownership != "verified":
			facts.PendingDomains = append(facts.PendingDomains, hostname)
		case strategy == "managed" && (certificate == "pending" || certificate == "issuing"):
			facts.PendingCertificates = append(facts.PendingCertificates, hostname)
		}
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return HealthFacts{}, translate(err)
	}
	return facts, nil
}

// explainHealth refines a projected health view with live facts. It never makes a healthy tunnel
// look worse than the facts support, and it only replaces the generic "pending" and "not ready"
// explanations with the specific cause and what to do about it.
func explainHealth(view HealthView, facts HealthFacts) HealthView {
	switch view.OverallCode {
	case "tunnel_expired", "tunnel_deleted", "tunnel_paused", "custom_domain_required":
		return view
	}
	set := func(code, summary, action string, retrying bool) HealthView {
		view.OverallCode, view.Summary, view.RepairAction, view.Retrying = code, summary, action, retrying
		return view
	}
	if facts.BandwidthExhausted {
		view.Dimensions.Access = HealthDimension{Status: "down", Code: "bandwidth_exhausted"}
		return set("bandwidth_exhausted", "Your workspace has used its bandwidth allowance, so traffic is not forwarded.",
			"Add bandwidth or change plan on the Billing page; traffic resumes when the allowance is available.", false)
	}
	if view.OverallCode == "ready" && !facts.EngineServed && facts.ActiveConnectors > 0 && facts.ConnectorSessions == 0 {
		view.OverallCode = "pending"
	}
	switch view.OverallCode {
	case "origin_unreachable":
		view.Dimensions.Origin = HealthDimension{Status: "down", Code: originCode(facts.OriginReason)}
		return set("origin_unreachable", originSummary(facts.OriginReason), originAction(facts.OriginReason), true)
	case "route_unavailable":
		view.Dimensions.Route = HealthDimension{Status: "down", Code: "route_rejected"}
		return set("route_unavailable", "The edge could not forward a tunnel route; its configuration was rejected or the origin address cannot be reached from the host.",
			"Run `pb tunnel route list` to review the route, fix its origin address, then update it with `pb tunnel route update`.", true)
	case "tunnel_pending", "pending":
	default:
		return view
	}
	switch {
	// A tunnel the engine serves has no connector session; a connector row left by tunnel creation
	// says nothing about it, so only the connector cases are skipped.
	case !facts.EngineServed && facts.ActiveConnectors == 0:
		view.Dimensions.Service = HealthDimension{Status: "down", Code: "connector_missing"}
		return set("connector_missing", "This tunnel has no connector, so nothing is serving it.",
			"Add a connector on the machine that runs the app: `pb tunnel connector add <tunnel>`.", false)
	case !facts.EngineServed && facts.ConnectorSessions == 0:
		view.Dimensions.Service = HealthDimension{Status: "down", Code: "connector_offline"}
		return set("connector_offline", "The Paperboat host that runs this tunnel is offline or its service is stopped.",
			"Make sure the machine is on and online (`pb environments`), then run `pb doctor` on it; the tunnel reconnects by itself.", true)
	case !facts.EdgeReady:
		view.Dimensions.Edge = HealthDimension{Status: "down", Code: "edge_unavailable"}
		return set("edge_unavailable", "The host is connected, but no Paperboat edge is ready to carry this tunnel's traffic.",
			"Nothing to change on your side. If this lasts more than a few minutes, run `pb tunnel doctor <tunnel>` and send the support reference.", true)
	case len(facts.PendingDomains) > 0:
		view.Dimensions.DNS = HealthDimension{Status: "down", Code: "dns_pending"}
		return set("dns_pending", "Waiting for DNS: "+facts.PendingDomains[0]+" is not verified yet.",
			"Add the DNS records shown by `pb tunnel domain instructions <tunnel> "+facts.PendingDomains[0]+"`, then run `pb tunnel domain verify`.", true)
	case len(facts.PendingCertificates) > 0:
		view.Dimensions.Certificate = HealthDimension{Status: "unknown", Code: "certificate_pending"}
		return set("certificate_pending", "The certificate for "+facts.PendingCertificates[0]+" is still being issued.",
			"Wait a few minutes; issuance starts once DNS is verified. `pb tunnel domain list <tunnel>` shows its state.", true)
	}
	return view
}

func originCode(reason string) string {
	switch reason {
	case "refused", "timeout", "unreachable", "tls":
		return "origin_" + reason
	}
	return "origin_unreachable"
}

func originSummary(reason string) string {
	switch reason {
	case "refused":
		return "The tunnel is connected, but the local app refused the connection: nothing is listening on its port."
	case "timeout":
		return "The tunnel is connected, but the local app did not answer in time."
	case "unreachable":
		return "The tunnel is connected, but the host cannot reach the local app's address."
	case "tls":
		return "The tunnel is connected, but the local app's TLS certificate was not accepted."
	}
	return "The tunnel is connected but its local app is not reachable."
}

func originAction(reason string) string {
	switch reason {
	case "refused":
		return "Start the app on the route's port (or fix the port with `pb tunnel route update`); the tunnel recovers within seconds."
	case "timeout":
		return "The app is running but not responding: check that it is healthy and not blocked by a firewall."
	case "unreachable":
		return "Check the route's origin address with `pb tunnel route list <tunnel>`; it must be reachable from the host."
	case "tls":
		return "Trust the app's certificate (route TLS settings: custom CA) or use an http origin for this route."
	}
	return "The connector cannot reach the app behind this tunnel. Start it, then wait a few seconds."
}

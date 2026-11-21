package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/accountingauthority"
	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"

	"github.com/pinksaucepasta/paperboat/server/internal/ingressprojection"
)

// HTTP01Source supplies the http-01 responses an edge must serve. ForEdge also
// records that the edge has fetched them, which the issuer waits for.
type HTTP01Source interface {
	ForEdge(nodeID string) []wire.HTTP01Response
}

// handleIngress is mounted only beneath the authenticated edge-control mux.
// A complete empty snapshot revokes authority; failed reads never renew it.
func (s *EdgeService) handleIngress(w http.ResponseWriter, r *http.Request) {
	var input struct {
		NodeID       string `json:"edge_node_id"`
		ProcessEpoch string `json:"process_epoch"`
		// Version is the version of the answer the edge holds; when it is still current only fresh
		// times come back (F21-38).
		Version string `json:"version,omitempty"`
	}
	if !s.decode(w, r, &input) {
		return
	}
	if wire.ValidateIdentifier(input.NodeID) != nil || wire.ValidateOpaqueEpoch(input.ProcessEpoch) != nil {
		writeEdgeError(w, r, http.StatusBadRequest, "invalid_request", false, 0)
		return
	}
	// Only a verified public edge is served ingress decisions. A relay, or an edge whose public ingress has not
	// been verified, gets a clear final answer instead of a retryable 5xx it would keep polling.
	if state, err := s.ingressNodeState(r.Context(), input.NodeID); err != nil {
		writeEdgeError(w, r, http.StatusServiceUnavailable, "control_unavailable", true, 1000, err)
		return
	} else if state != ingressNodeVerifiedEdge {
		writeEdgeError(w, r, http.StatusConflict, state, false, 0)
		return
	}
	decisions, throttled, blocked, ipRules, offline, err := s.ingressProjection(r.Context(), input.NodeID, input.ProcessEpoch, false)
	if err != nil {
		writeEdgeError(w, r, http.StatusServiceUnavailable, "control_unavailable", true, 1000, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var responses []wire.HTTP01Response
	// The field exists only where http-01 is configured (staging).
	if s.http01 != nil {
		responses = s.http01.ForEdge(input.NodeID)
	}
	version := wire.IngressVersion(decisions, throttled, blocked, ipRules, offline, responses)
	if input.Version == version {
		expires := make([]time.Time, len(decisions))
		for i, d := range decisions {
			expires[i] = d.ExpiresAt
		}
		writeEdgeSnapshot(w, r, map[string]any{"complete": true, "unchanged": true, "version": version, "issued_at": latestIssuedAt(decisions), "expires_at": expires})
		return
	}
	snapshotBody := map[string]any{"complete": true, "version": version, "decisions": decisions, "throttled": throttled, "blocked": blocked, "ip_rules": ipRules, "offline": offline}
	if s.http01 != nil {
		snapshotBody["http01_responses"] = responses
	}
	writeEdgeSnapshot(w, r, snapshotBody)
}

// ingressProjection is the database work behind one desired-state answer: the node's decisions, the throttles
// and takedowns that withhold some of them, the owners' IP rules and the previews whose owner is offline.
func (s *EdgeService) ingressProjection(ctx context.Context, nodeID, epoch string, readOnly bool) (decisions []wire.IngressDecision, throttled []wire.IngressThrottle, blocked []wire.IngressBlock, ipRules []wire.IngressIPRule, offline []wire.IngressOffline, err error) {
	quota, _ := s.scopedBandwidth.(accountingauthority.QuotaChecker)
	decisions, err = (ingressprojection.SQLIngressSource{DB: s.store, Quota: quota, Throttled: &throttled, ReadOnly: readOnly}).Edge(ctx, nodeID, epoch)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if blocked, err = withholdBlocked(ctx, s.store, &decisions, &throttled); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	// A customer-owned edge carries no Paperboat fair-use slowdown and no browser-terminal cap.
	customerOwned, err := s.customerOwnedNode(ctx, nodeID)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if customerOwned {
		throttled = nil
	} else {
		attached, err := s.attachedTerminals(ctx, nodeID)
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}
		throttled = s.addFileThrottles(ctx, attached, throttled)
		if err = s.assignThrottleRates(ctx, throttled); err != nil {
			return nil, nil, nil, nil, nil, err
		}
		throttled = s.addTerminalCaps(attached, throttled)
	}
	if ipRules, err = ownerIPRules(ctx, s.store, decisions); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	offline, err = offlineOwnerPreviews(ctx, s.store, nodeID)
	return decisions, throttled, blocked, ipRules, offline, err
}

// attachedTerminal is one runtime hostname with a console browser terminal attached on this edge, the workspace that pays for it and the
// number of edges that workspace's terminals are attached on.
type attachedTerminal struct {
	workspace, host string
	edges           int64
}

// attachedTerminals lists the console browser terminals attached on this edge. Terminals have no ingress decision of their own; the
// attachments (native usage authorities, renewed while attached) say which workspaces and hosts to list.
func (s *EdgeService) attachedTerminals(ctx context.Context, nodeID string) ([]attachedTerminal, error) {
	rows, err := s.store.SQL().QueryContext(ctx, `SELECT DISTINCT n.workspace_id,r.public_host,
 (SELECT count(DISTINCT a.edge_node_id) FROM paperboat.native_usage_authorities a WHERE a.workspace_id=n.workspace_id AND a.resource_kind='terminal' AND coalesce(a.attached_until,a.expires_at)>now())
FROM paperboat.native_usage_authorities n
JOIN paperboat.user_machines m ON m.id=n.machine_id
JOIN paperboat.control_routes r ON r.environment_id=m.environment_id AND r.kind='runtime_https_wss' AND r.applied_node_id=$1
WHERE n.resource_kind='terminal' AND n.edge_node_id=$1 AND coalesce(n.attached_until,n.expires_at)>now()
`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []attachedTerminal
	for rows.Next() {
		var t attachedTerminal
		if err := rows.Scan(&t.workspace, &t.host, &t.edges); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// fileThrottleHost is the hostname a terminal's FILE transfers are listed and paced under: apart from the terminal's own entry
// (its anti-abuse cap), so the two buckets never mix.
func fileThrottleHost(host string) string { return "files:" + host }

// addFileThrottles lists, for each workspace past its included traffic that has a terminal attached here, the terminal's file-transfer key,
// so the edge paces the files the browser tags (see edgehttp.terminalPacing) with the workspace's allowance bucket like its other
// public traffic. The gate is cached and fails open (a slow database never throttles an upload). Typing and terminal output are not
// listed: they are neither counted nor slowed by the allowance.
func (s *EdgeService) addFileThrottles(ctx context.Context, attached []attachedTerminal, throttled []wire.IngressThrottle) []wire.IngressThrottle {
	now := s.clock()
	for _, t := range attached {
		if s.workspaceGate.Denied(ctx, t.workspace, now) {
			throttled = append(throttled, wire.IngressThrottle{Hostname: fileThrottleHost(t.host), Workspace: t.workspace})
		}
	}
	return throttled
}

// addTerminalCaps lists each attached terminal's runtime hostname with this edge's share of the workspace's terminal cap
// (config.FairUse.TerminalMbit divided by the edges the workspace's terminals are attached on), so all the workspace's terminal traffic
// together stays under the cap however many sessions or edges carry it. The cap is anti-abuse only.
func (s *EdgeService) addTerminalCaps(attached []attachedTerminal, throttled []wire.IngressThrottle) []wire.IngressThrottle {
	capBytes := int64(s.fairUse.TerminalMbit * 125_000)
	for _, t := range attached {
		throttled = append(throttled, wire.IngressThrottle{Hostname: t.host, Workspace: t.workspace, BytesPerSecond: max(capBytes/max(t.edges, 1), minThrottleBytesPerSecond), Terminal: true})
	}
	return throttled
}

// minThrottleBytesPerSecond keeps one edge's share of a throttled workspace able to move a request at all.
const minThrottleBytesPerSecond = 1024

// assignThrottleRates gives each throttle entry this edge's share of its workspace's slow speed: Free's or a paid plan's
// rate (config.FairUse) divided by the number of edges that carry the workspace, so the workspace as a whole stays at the
// plan's rate however many edges serve it (an idle edge's share is not handed to the busy one).
// shortcut: the share is fixed per snapshot, not rebalanced by live use; rebalance if workspaces regularly span edges unevenly.
func (s *EdgeService) assignThrottleRates(ctx context.Context, throttled []wire.IngressThrottle) error {
	rates := map[string]int64{}
	for i := range throttled {
		workspace := throttled[i].Workspace
		rate, known := rates[workspace]
		if !known {
			var paid bool
			var edges int64
			if err := s.store.SQL().QueryRowContext(ctx, `SELECT
 coalesce((SELECT w.product_code IS NOT NULL AND billing_workspace_paid_until(w)>now() FROM billing_workspaces w WHERE w.id=$1),false),
 (SELECT count(DISTINCT a.edge_node_id) FROM usage_accounting_authorities a WHERE a.workspace_id=$1 AND a.admission_expires_at>now())`, workspace).Scan(&paid, &edges); err != nil {
				return err
			}
			base := s.fairUse.EdgeFreeBytesPerSecond
			if paid {
				base = s.fairUse.EdgeBytesPerSecond
			}
			rate = max(base/max(edges, 1), minThrottleBytesPerSecond)
			rates[workspace] = rate
		}
		throttled[i].BytesPerSecond = rate
	}
	return nil
}

// offlineOwnerPreviews lists the hostnames of active foreground previews whose owner machine is offline or
// asleep (not revoked or deleted), or that this edge holds no live route for (the carrier dropped, ~30 s, long
// before the machine is marked offline at 90 s): their lease persists (it ends only when the command stops or
// the machine is removed), so the edge answers their URL with the "machine offline" page instead of a 404. It
// grants no authority. A background or lazy preview keeps its own rules and is not listed.
func offlineOwnerPreviews(ctx context.Context, store *db.DB, nodeID string) ([]wire.IngressOffline, error) {
	rows, err := store.Pool().Query(ctx, `SELECT publication.hostname FROM preview_leases p
JOIN users owner ON owner.id=p.account_id AND owner.status='active'
JOIN user_machines m ON m.id=p.owner_machine_id
JOIN LATERAL (
 SELECT substring(p.endpoint from 9) hostname WHERE paperboat.workspace_edge_mode(p.account_id,p.owner_team_id)='managed-only'
 UNION ALL
 SELECT d.hostname FROM preview_domains d WHERE d.preview_id=p.id AND d.account_id=p.account_id
 AND d.deleted_at IS NULL AND paperboat.workspace_domain_verified(d.workspace_domain_id) AND d.conflict_state='clear'
 AND d.certificate_state='ready' AND d.certificate_reference IS NOT NULL AND d.preview_generation=p.generation
) publication ON true
WHERE p.terminal_state='active'
 AND (p.owner_session_kind='foreground' OR (p.owner_session_kind='local_lease' AND p.user_deadline IS NULL))
 AND m.revoked_at IS NULL AND m.deleted_at IS NULL
 AND (NOT (m.state='online' AND m.online) OR NOT EXISTS (SELECT 1 FROM endpoint_edge_assignments x
  WHERE x.endpoint_id=p.id AND x.edge_node_id=$1 AND x.observed_ready AND x.observed_at>now()-interval '2 minutes'))
 AND paperboat.node_preview_eligible($1,p.account_id,p.id)
ORDER BY p.id,publication.hostname`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	offline := []wire.IngressOffline{}
	for rows.Next() {
		var o wire.IngressOffline
		if err := rows.Scan(&o.Hostname); err != nil {
			return nil, err
		}
		offline = append(offline, o)
	}
	return offline, rows.Err()
}

// Warm answers the desired-state projection internally for the verified edges, parallel times at once, so
// every pooled connection has prepared its statements before the replacement server reports ready. It reads
// only: no accounting binding is retained, so it never takes an edge's accounting lock. A server with no verified edge has nothing to warm.
func (s *EdgeService) Warm(ctx context.Context, parallel int) error {
	rows, err := s.store.SQL().QueryContext(ctx, `SELECT id, process_epoch FROM control_tunnel_nodes WHERE roles @> ARRAY['edge']::text[] AND public_ingress_verified_at IS NOT NULL ORDER BY last_heartbeat_at DESC NULLS LAST LIMIT 3`)
	if err != nil {
		return fmt.Errorf("list edges to warm: %w", err)
	}
	type node struct{ id, epoch string }
	var nodes []node
	for rows.Next() {
		var n node
		if err := rows.Scan(&n.id, &n.epoch); err != nil {
			rows.Close()
			return err
		}
		nodes = append(nodes, n)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var wg sync.WaitGroup
	failures := make(chan error, len(nodes)*parallel)
	for _, n := range nodes {
		for range parallel {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, _, _, _, err := s.ingressProjection(ctx, n.id, n.epoch, true); err != nil {
					failures <- err
				}
			}()
		}
	}
	wg.Wait()
	close(failures)
	return <-failures
}

// latestIssuedAt is the newest issue time of the answer; a kept decision never gets a longer window.
func latestIssuedAt(decisions []wire.IngressDecision) (latest time.Time) {
	for _, d := range decisions {
		if d.IssuedAt.After(latest) {
			latest = d.IssuedAt
		}
	}
	return latest
}

const (
	ingressNodeVerifiedEdge = "verified_edge"
	ingressNodeNotEdge      = "node_not_edge"
	ingressNodeNotVerified  = "node_not_verified"
)

// ingressNodeState says whether the node may be served ingress decisions: a node with the edge role whose public
// ingress address has been verified. Unknown nodes are reported as not an edge.
func (s *EdgeService) ingressNodeState(ctx context.Context, nodeID string) (string, error) {
	var isEdge, verified bool
	err := s.store.SQL().QueryRowContext(ctx, `SELECT roles @> ARRAY['edge']::text[], public_ingress_verified_at IS NOT NULL FROM control_tunnel_nodes WHERE id=$1`, nodeID).Scan(&isEdge, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return ingressNodeNotEdge, nil
	}
	if err != nil {
		return "", err
	}
	switch {
	case !isEdge:
		return ingressNodeNotEdge, nil
	case !verified:
		return ingressNodeNotVerified, nil
	}
	return ingressNodeVerifiedEdge, nil
}

// withholdBlocked removes the decisions and throttles of hostnames an operator took down
// (publication_blocks) and returns those hostnames, so the edge answers the takedown page without
// contacting the origin. A name is listed only when this edge has a decision for it. The
// hostnames of previews and tunnels owned by a banned account or team are listed whether or not
// this edge holds a decision for them: their machines lost access, so a decision may be gone already,
// and the edge must still answer the takedown page rather than "this address does not exist".
func withholdBlocked(ctx context.Context, store *db.DB, decisions *[]wire.IngressDecision, throttled *[]wire.IngressThrottle) ([]wire.IngressBlock, error) {
	names := make([]string, 0, len(*decisions))
	for _, d := range *decisions {
		names = append(names, d.Binding.Hostname)
	}
	blocked := []wire.IngressBlock{}
	taken := map[string]bool{}
	if len(names) > 0 {
		rows, err := store.Pool().Query(ctx, `SELECT hostname, legal FROM publication_blocks WHERE blocked AND hostname = ANY($1::text[])`, names)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var b wire.IngressBlock
			if err := rows.Scan(&b.Hostname, &b.Legal); err != nil {
				return nil, err
			}
			taken[b.Hostname] = true
			blocked = append(blocked, b)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	// A banned owner's name is never served: those among this edge's decisions are withheld whatever the list's size.
	withheld, err := bannedAmong(ctx, store, names)
	if err != nil {
		return nil, err
	}
	listed, err := bannedOwnerHostnames(ctx, store)
	if err != nil {
		return nil, err
	}
	for _, host := range append(withheld, listed...) {
		if !taken[host] {
			taken[host] = true
			blocked = append(blocked, wire.IngressBlock{Hostname: host})
		}
	}
	*decisions = slices.DeleteFunc(*decisions, func(d wire.IngressDecision) bool { return taken[d.Binding.Hostname] })
	*throttled = slices.DeleteFunc(*throttled, func(d wire.IngressThrottle) bool { return taken[d.Hostname] })
	return blocked, nil
}

const bannedOwnerHostnamesSQL = `SELECT DISTINCT hostname FROM (
 SELECT r.hostname FROM endpoint_routes r JOIN endpoints e ON e.id=r.endpoint_id AND e.desired_state='active'
  JOIN bans b ON b.lifted_at IS NULL AND ((b.subject_kind='user' AND b.subject_id=e.account_id) OR (b.subject_kind='team' AND b.subject_id=e.owner_team_id))
 UNION ALL
 SELECT substring(p.endpoint from 9) FROM preview_leases p
  JOIN bans b ON b.lifted_at IS NULL AND ((b.subject_kind='user' AND b.subject_id=p.account_id) OR (b.subject_kind='team' AND b.subject_id=p.owner_team_id))
  WHERE p.terminal_state='active' AND p.endpoint LIKE 'https://%'
 UNION ALL
 SELECT d.hostname FROM tunnel_domains d JOIN tunnels t ON t.id=d.tunnel_id AND t.account_id=d.account_id
  JOIN bans b ON b.lifted_at IS NULL AND ((b.subject_kind='user' AND b.subject_id=t.account_id) OR (b.subject_kind='team' AND b.subject_id=t.owner_team_id))
  WHERE d.deleted_at IS NULL
 UNION ALL
 SELECT d.hostname FROM preview_domains d JOIN preview_leases p ON p.id=d.preview_id AND p.account_id=d.account_id
  JOIN bans b ON b.lifted_at IS NULL AND ((b.subject_kind='user' AND b.subject_id=p.account_id) OR (b.subject_kind='team' AND b.subject_id=p.owner_team_id))
  WHERE p.terminal_state='active' AND d.deleted_at IS NULL
) h WHERE hostname IS NOT NULL`

// bannedOwnerHostnames lists the hostnames of the previews and tunnels of accounts and teams under a running ban.
func bannedOwnerHostnames(ctx context.Context, store *db.DB) ([]string, error) {
	return hostnames(ctx, store, bannedOwnerHostnamesSQL+` ORDER BY hostname`)
}

// bannedAmong returns those of names whose owner is under a running ban.
func bannedAmong(ctx context.Context, store *db.DB, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	return hostnames(ctx, store, bannedOwnerHostnamesSQL+` AND hostname = ANY($1::text[])`, names)
}

func hostnames(ctx context.Context, store *db.DB, query string, args ...any) ([]string, error) {
	rows, err := store.Pool().Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return nil, err
		}
		out = append(out, host)
	}
	return out, rows.Err()
}

// ownerIPRules lists the owner's IP allow and deny lists (access_ip_rules) for the hostnames of this
// edge's decisions: a preview's rules key on its lease (the publication), a tunnel's on the tunnel. A durable
// decision's Binding.TunnelID is the runtime carrier's identity, not the tunnel's, so the tunnel is found
// through the route the decision names.
func ownerIPRules(ctx context.Context, store *db.DB, decisions []wire.IngressDecision) ([]wire.IngressIPRule, error) {
	rules := []wire.IngressIPRule{}
	if len(decisions) == 0 {
		return rules, nil
	}
	var previews, routes []string
	for _, d := range decisions {
		if d.Binding.Lifecycle == wire.TunnelEphemeral {
			previews = append(previews, d.Binding.PublicationID)
		} else {
			routes = append(routes, d.Binding.RouteID)
		}
	}
	rows, err := store.Pool().Query(ctx, `SELECT 'preview', resource_id, allow_cidrs, deny_cidrs FROM access_ip_rules
WHERE resource_kind='preview' AND resource_id = ANY($1::text[])
UNION ALL
SELECT 'route', r.id, a.allow_cidrs, a.deny_cidrs FROM tunnel_routes r
JOIN access_ip_rules a ON a.resource_kind='tunnel' AND a.resource_id=r.tunnel_id
WHERE r.id = ANY($2::text[])`, previews, routes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type lists struct{ allow, deny []string }
	byResource := map[[2]string]lists{}
	for rows.Next() {
		var kind, id string
		var l lists
		if err := rows.Scan(&kind, &id, &l.allow, &l.deny); err != nil {
			return nil, err
		}
		byResource[[2]string{kind, id}] = l
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	free, err := freeOwnedResources(ctx, store, previews, routes)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, d := range decisions {
		key := [2]string{"route", d.Binding.RouteID}
		if d.Binding.Lifecycle == wire.TunnelEphemeral {
			key = [2]string{"preview", d.Binding.PublicationID}
		}
		l, ok := byResource[key]
		if (!ok && !free[key]) || seen[d.Binding.Hostname] {
			continue
		}
		seen[d.Binding.Hostname] = true
		rules = append(rules, wire.IngressIPRule{Hostname: d.Binding.Hostname, Allow: l.allow, Deny: l.deny, FreeRequests: free[key]})
	}
	return rules, nil
}

// freeOwnedResources picks, from the previews (lease ids) and tunnel routes (route ids) of an edge's decisions, those a
// Free account owns: not owned by a team, and the owner has no paid personal plan (the plan test of planlimits.planTx).
// The edge applies the Free request rates to exactly these (edgelimits.FreeRequests).
func freeOwnedResources(ctx context.Context, store *db.DB, previews, routes []string) (map[[2]string]bool, error) {
	const personalProduct = `coalesce((SELECT w.product_code FROM billing_workspaces w
 WHERE w.user_id=%s AND w.team_id IS NULL AND billing_workspace_paid_until(w)>now()),'') NOT LIKE 'pro-%%'`
	rows, err := store.Pool().Query(ctx, `SELECT 'preview', p.id FROM preview_leases p
WHERE p.id = ANY($1::text[]) AND p.owner_team_id IS NULL AND `+fmt.Sprintf(personalProduct, "p.account_id")+`
UNION ALL
SELECT 'route', r.id FROM tunnel_routes r JOIN tunnels t ON t.id=r.tunnel_id
WHERE r.id = ANY($2::text[]) AND t.owner_team_id IS NULL AND `+fmt.Sprintf(personalProduct, "t.account_id"), previews, routes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	free := map[[2]string]bool{}
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			return nil, err
		}
		free[[2]string{kind, id}] = true
	}
	return free, rows.Err()
}
